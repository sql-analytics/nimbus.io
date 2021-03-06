#!/bin/bash

PROG_DIR="$(cd $(dirname $0) ; pwd)"

if [[ ! -z "$NIMBUSIO_WILDCARD_SSL_CERT" ]]; then
    exec tproxy \
        -n web_director \
        -w 4 \
        -b "${NIMBUSIO_WEB_DIRECTOR_ADDR:?}:${NIMBUSIO_WEB_DIRECTOR_PORT:?}" \
        --ssl-certfile "${NIMBUSIO_WILDCARD_SSL_CERT?:}" \
        --ssl-keyfile "${NIMBUSIO_WILDCARD_SSL_KEY?:}" \
        $PROG_DIR/web_director_main.py 2>&1
else
    exec tproxy \
        -n web_director \
        -w 4 \
        -b ${NIMBUS_IO_SERVICE_DOMAIN:?}:${NIMBUSIO_WEB_PUBLIC_READER_PORT:?} \
        $PROG_DIR/web_director_main.py 2>&1
fi

