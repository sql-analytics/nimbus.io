"""
very basic prototype for web director for nimbus.io

all it does so far is try to get traffic to one of the hosts in the appropriate
cluster

does not implement several of the needed capabilities including:
    - health checks for hosts or listen for a heartbeat
    - distinguish between read and write requests and send to different
      destinations
    - pick read hosts based on key consistent hashing for caching
    - pick write hosts based on least busy (busyness info included in
      heartbeats)
    - handle key authentication for reads
    - handle requests for public collections w/o auth
    - warm the cache on startup (select all the clusters, and the mapping of
      collections to clusters in bulk from the database.)
    - expiration of cached values (i.e. when a collections information changes,
      such as becoming public, private, or being migrated to a different
      cluster.)
"""

import traceback
from functools import wraps
import time
import logging
import os
import re
from collections import deque
import gevent
from gevent.coros import RLock as Lock
from gevent.event import Event

import psycopg2
import gevent_psycopg2
gevent_psycopg2.monkey_patch()

from tools.standard_logging import initialize_logging
from tools.LRUCache import LRUCache
from tools.database_connection import retry_central_connection

# how long to wait before returning an error message to avoid fast loops
RETRY_DELAY = 0.0
# LRUCache mapping names to integers is approximately 32m of memory per 100,000
# entries
COLLECTION_CACHE_SIZE = 500000
NIMBUS_IO_SERVICE_DOMAIN = os.environ['NIMBUS_IO_SERVICE_DOMAIN']
NIMBUSIO_WEB_SERVER_PORT = int(os.environ['NIMBUSIO_WEB_SERVER_PORT'])
NIMBUSIO_MANAGEMENT_API_REQUEST_DEST = \
    os.environ['NIMBUSIO_MANAGEMENT_API_REQUEST_DEST']

_log_path = "%s/nimbusio_web_director.log" % (os.environ["NIMBUSIO_LOG_DIR"], )

def _supervise_db_interaction(bound_method):
    """
    Decorator for methods of Router class (below) to manage locks and
    reconnections to database
    """
    @wraps(bound_method)
    def __supervise_db_interaction(instance, *args, **kwargs):
        log = logging.getLogger("supervise_db")
        lock = instance.dblock
        retries = 0
        start_time = time.time()

        # it maybe that some other greenlet has got here first, and already
        # updated our cache to include the item that we are querying. In some
        # situations, such as when the database takes a few seconds to respond,
        # or when the database is offline, there maybe many greenlets waiting,
        # all to query the database for the same result.  To avoid this
        # thundering herd of database hits of likely cached values, the caller
        # may supply us with a cache check function.
        cache_check_func = None
        if 'cache_check_func' in kwargs:
            cache_check_func = kwargs.pop('cache_check_func') 

        while True:
            if retries:
                # do not retry too fast
                time.sleep(1.0)
            with lock:
                conn_id = id(instance.conn)
                try:
                    if cache_check_func is not None:
                        result = cache_check_func()
                        if result:
                            break
                    result = bound_method(instance, *args, **kwargs)
                    break
                except psycopg2.OperationalError, err:
                    log.warn("Database error %s %s (retry #%d)" % (
                        getattr(err, "pgcode", '-'),
                        getattr(err, "pgerror", '-'),
                        retries, ))
                    retries += 1
                    # only let one greenlet be retrying the connection
                    # only reconnect if some other greenlet hasn't already done
                    # so.
                    log.warn("replacing database connection %r" % (
                        conn_id, ))
                    try:
                        if instance.conn is not None:
                            instance.conn.close()
                    except psycopg2.OperationalError, err2:
                        pass
                    instance.conn = retry_central_connection()
                    conn_id = id(instance.conn)
        return result
    return __supervise_db_interaction

class Router(object):
    """
    Router object for assisting the proxy function (below.)
    Holds database connection, state for caching, etc.
    """

    def __init__(self):
        self.init_complete = Event()
        self.conn = None
        self.dblock = Lock()
        self.service_domain = NIMBUS_IO_SERVICE_DOMAIN
        self.dest_port = NIMBUSIO_WEB_SERVER_PORT
        self.known_clusters = dict()
        self.known_collections = LRUCache(COLLECTION_CACHE_SIZE) 
        self.management_api_request_dest_hosts = \
            deque(NIMBUSIO_MANAGEMENT_API_REQUEST_DEST.strip().split())

    def init(self):
        #import logging
        #import traceback
        #from tools.database_connection import get_central_connection
        log = logging.getLogger("init")
        log.info("init start")
        self.conn = retry_central_connection()
        log.info("init complete")
        self.init_complete.set()

    def _parse_collection(self, hostname):
        "return the Nimbus.io collection name from host name"
        offset = -1 * ( len(self.service_domain) + 1 )
        return hostname[:offset]

    def _hosts_for_collection(self, collection):
        "return a list of hosts for this collection"
        cluster_id = self._cluster_for_collection(collection)
        if cluster_id is None:
            return None
        cluster_info = self._cluster_info(cluster_id)
        return cluster_info['hosts']

    @_supervise_db_interaction
    def _db_cluster_for_collection(self, collection):
        # FIXME how do we handle null result here? do we just cache the null
        # result?
        row = self.conn.fetch_one_row(
            "select cluster_id from nimbusio_central.collection where name=%s",
            [collection, ])
        if row:
            return row[0]

    @_supervise_db_interaction
    def _cluster_for_collection(self, collection, _retries=0):
        "return cluster ID for collection"
        if collection in self.known_collections:
            return self.known_collections[collection]
        result = self._db_cluster_for_collection(collection,
            cache_check_func = 
                lambda: self.known_collections.get(collection, None))
        self.known_collections[collection] = result
        return result
            
    @_supervise_db_interaction
    def _db_cluster_info(self, cluster_id):
        rows = self.conn.fetch_all_rows("""
            select name, hostname, node_number_in_cluster 
            from nimbusio_central.node 
            where cluster_id=%s 
            order by node_number_in_cluster""", 
            [cluster_id, ])
    
        info = dict(rows = list(rows), 
                    hosts = deque([r[1] for r in rows]))

        return info

    def _cluster_info(self, cluster_id):
        "return info about a cluster and its hosts"
        if cluster_id in self.known_clusters:
            return self.known_clusters[cluster_id]
        
        info = self._db_cluster_info(cluster_id, 
            cache_check_func=lambda: self.known_clusters.get(cluster_id, None))
        
        self.known_clusters[cluster_id] = info 
        return info

    @staticmethod
    def _reject(code, reason):
        "return a go away response"
        log = logging.getLogger("reject")
        response = "%d %s" % (code, reason, )
        log.debug("reject:" + response)
        return dict(close = response)

    def route(self, hostname):
        """
        route a to a host in the appropriate cluster, using simple round-robin
        among the hosts in a cluster
        """
        log = logging.getLogger("route")

        self.init_complete.wait()

        if not hostname.endswith(self.service_domain):
            return self._reject(404, "Not found")

        if hostname == self.service_domain:
            # this is not a request specific to any particular collection
            # TODO figure out how to route these requests.
            # in production, this might not matter.
            self.management_api_request_dest_hosts.rotate(1)
            target = self.management_api_request_dest_hosts[0]
            log.debug("routing management request to backend host %s" %
                (target, ))
            return dict(remote = target)


        collection = self._parse_collection(hostname)
        if collection is None:
            self._reject(404, "Collection not found")

        hosts = self._hosts_for_collection(collection)
        if hosts:
            # simple round robin rouding
            hosts.rotate(1)
            target = hosts[0]
            log.debug("routing connection to %s to backend host %s" % 
                (hostname, target, ))
            return dict(remote = "%s:%d" % (target, self.dest_port, ))
        elif hosts is None:
            self._reject(404, "Collection not found")

        # no hosts currently available (hosts is an empty list, presumably)
        gevent.sleep(RETRY_DELAY)
        return self._reject(500, "Retry later")

_ROUTER = Router()

def proxy(data, _re_host=re.compile("Host:\s*(.*?)(:(\d+))?\r\n")):
    """
    the function called by tproxy to determine where to send traffic

    tproxy will call this function repeatedly for the same connection, as we
    receive more incoming data, until we return something other than None.

    typically our response tells tproxy where to proxy the connection to, but
    may also tell it to hang up, or respond with some error message.
    """

    log = logging.getLogger("proxy")
    # match against the HTTP host headers to determine routing by collection
    # name
    matches = _re_host.findall(data)
    if matches:
        hostname, _, port = matches.pop()
        port = int(port)
        log.debug("trying to route %r" % (hostname, ))
        try:
            return _ROUTER.route(hostname)
            #return dict(remote = "bonsai2:9000")
        except Exception, err:
            log.error("error routing %s:%d: %s" % (
                hostname, port, traceback.format_exc(), ))
            gevent.sleep(RETRY_DELAY)
            return dict(close=True)
    elif len(data) > 4096:
        # we should have had a host header by now...
        return dict(close=True)
    else:
        # wait until we have more data
        return None

def init_setup():
    initialize_logging(_log_path)
    log = logging.getLogger("init_setup")
    log.info("setup start")
    global _ROUTER
    _ROUTER = Router()
    gevent.spawn_later(0.0, _ROUTER.init)
    log.info("setup complete")

init_setup()
