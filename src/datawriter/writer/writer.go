package writer

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"tools"

	"datawriter/logger"
	"datawriter/msg"
	"datawriter/nodedb"
	"datawriter/types"
)

type NimbusioWriter interface {

	// StartSegment initializes a new segment and prepares to receive data
	// for it
	StartSegment(lgr logger.Logger, segment msg.Segment,
		nodeNames msg.NodeNames) error

	// StoreSequence stores data for  an initialized segment
	StoreSequence(lgr logger.Logger, segment msg.Segment,
		sequence msg.Sequence, data []byte) error
	/*
		// CancelSegment stops processing the segment
		CancelSegment(lgr logger.Logger, cancel msg.Cancel) error
	*/
	// FinishSegment finishes storing the segment
	FinishSegment(lgr logger.Logger, segment msg.Segment,
		file msg.File) error
	/*
		// DestroyKey makes a key inaccessible
		DestroyKey(lgr logger.Logger, segment msg.Segment,
			unifiedIDToDestroy uint64) error

		// StartConjoinedArchive begins a conjoined archive
		StartConjoinedArchive(lgr logger.Logger,
			conjoinedEntry types.ConjoinedEntry) error

		// AbortConjoinedArchive cancels conjoined archive
		AbortConjoinedArchive(lgr logger.Logger,
			conjoinedEntry types.ConjoinedEntry) error

		// FinishConjoinedArchive completes a conjoined archive
		FinishConjoinedArchive(lgr logger.Logger,
			conjoinedEntry types.ConjoinedEntry) error
	*/
}

type segmentKey struct {
	UnifiedID     uint64
	ConjoinedPart uint32
	SegmentNum    uint8
}

func (key segmentKey) String() string {
	return fmt.Sprintf("(%d, %d, %d)", key.UnifiedID, key.ConjoinedPart,
		key.SegmentNum)
}

type segmentMapEntry struct {
	SegmentID      uint64
	LastActionTime time.Time
}

// map data contained in messages onto our internal segment id
type nimbusioWriter struct {
	NodeIDMap        map[string]uint32
	SegmentMap       map[segmentKey]segmentMapEntry
	FileSpaceInfo    tools.FileSpaceInfo
	ValueFile        OutputValueFile
	MaxValueFileSize uint64
}

// NewNimbusioWriter returns an entity that implements the NimbusioWriter interface
func NewNimbusioWriter() (NimbusioWriter, error) {
	var err error
	var writer nimbusioWriter
	writer.SegmentMap = make(map[segmentKey]segmentMapEntry)

	if writer.NodeIDMap, err = tools.GetNodeIDMap(); err != nil {
		return nil, fmt.Errorf("tools.GetNodeIDMap() failed %s", err)
	}

	maxValueFileSizeStr := os.Getenv("NIMBUS_IO_MAX_VALUE_FILE_SIZE")
	if maxValueFileSizeStr == "" {
		writer.MaxValueFileSize = uint64(1024 * 1024 * 1024)
	} else {
		var intSize int
		intSize, err = strconv.Atoi(maxValueFileSizeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid NIMBUS_IO_MAX_VALUE_FILE_SIZE '%s'",
				maxValueFileSizeStr)
		}
		writer.MaxValueFileSize = uint64(intSize)
	}

	if writer.FileSpaceInfo, err = tools.NewFileSpaceInfo(nodedb.NodeDB); err != nil {
		return nil, err
	}

	if writer.ValueFile, err = NewOutputValueFile(writer.FileSpaceInfo); err != nil {
		return nil, err
	}

	return &writer, nil
}

func (writer *nimbusioWriter) StartSegment(lgr logger.Logger,
	segment msg.Segment, nodeNames msg.NodeNames) error {

	var entry segmentMapEntry
	var err error
	var sourceNodeID uint32
	var ok bool

	lgr.Debug("StartSegment")

	if sourceNodeID, ok = writer.NodeIDMap[nodenames.SourceNodeName]; !ok {
		return fmt.Errorf("unknown source node %s", nodenames.SourceNodeName)
	}

	if nodeNames.HandoffNodeName != "" {
		if handoffNodeID, ok = writer.NodeIDMap[nodenames.HandoffNodeName]; !ok {
			return fmt.Errorf("unknown handoff node %s", nodenames.HandoffNodeName)
		}

		stmt := nodedb.Stmts["new-segment-for-handoff"]
		row := stmt.QueryRow(
			segment.CollectionID,
			segment.Key,
			segment.UnifiedID,
			entry.Timestamp,
			entry.SegmentNum,
			entry.ConjoinedPart,
			sourceNodeID,
			handoffNodeID)
		err = row.Scan(&segmentID)
	} else {
		stmt := nodedb.Stmts["new-segment"]
		row := stmt.QueryRow(
			entry.CollectionID,
			entry.Key,
			entry.UnifiedID,
			entry.Timestamp,
			entry.SegmentNum,
			entry.ConjoinedPart,
			sourceNodeID)
		err = row.Scan(&segmentID)
	}
	if entry.SegmentID, err = NewSegment(segment); err != nil {
		return err
	}
	entry.LastActionTime = tools.Timestamp()

	key := segmentKey{segment.UnifiedID, segment.ConjoinedPart,
		segment.SegmentNum}

	writer.SegmentMap[key] = entry

	return nil
}

func (writer *nimbusioWriter) StoreSequence(lgr logger.Logger,
	segment msg.Segment,
	sequence msg.Sequence, data []byte) error {
	var err error

	lgr.Debug("StoreSequence #%d", sequence.SequenceNum)

	if writer.ValueFile.Size()+sequence.SegmentSize >= writer.MaxValueFileSize {
		lgr.Info("value file full")
		if err = writer.ValueFile.Close(); err != nil {
			return fmt.Errorf("error closing value file %s", err)
		}
		if writer.ValueFile, err = NewOutputValueFile(writer.FileSpaceInfo); err != nil {
			return fmt.Errorf("error opening value file %s", err)
		}
	}

	key := segmentKey{segment.UnifiedID, segment.ConjoinedPart,
		segment.SegmentNum}
	entry, ok := writer.SegmentMap[key]
	if !ok {
		return fmt.Errorf("StoreSequence unknown segment %s", key)
	}

	// we need to store new-segment-sequence in the database before
	// ValueFile.Store, because we are using  writer.ValueFile.Size()
	// as the offset

	stmt := nodedb.Stmts["new-segment-sequence"]
	_, err = stmt.Exec(
		segment.CollectionID,
		entry.SegmentID,
		sequence.ZfecPaddingSize,
		writer.ValueFile.ID(),
		sequence.SequenceNum,
		writer.ValueFile.Size(),
		sequence.SegmentSize,
		sequence.MD5Digest,
		sequence.Adler32)
	if err != nil {
		return fmt.Errorf("new-segment-sequence %s", err)
	}

	err = writer.ValueFile.Store(segment.CollectionID, entry.SegmentID,
		data)
	if err != nil {
		return fmt.Errorf("ValueFile.Store %s", err)
	}

	entry.LastActionTime = tools.Timestamp()
	writer.SegmentMap[key] = entry

	return nil
}

// CancelSegment stops storing the segment
func (writer *nimbusioWriter) CancelSegment(lgr logger.Logger,
	cancel msg.Cancel) error {
	var err error

	lgr.Debug("CancelSegment")

	key := segmentKey{cancel.UnifiedID, cancel.ConjoinedPart,
		cancel.SegmentNum}
	delete(writer.SegmentMap, key)

	stmt := nodedb.Stmts["cancel-segment"]
	_, err = stmt.Exec(
		cancel.UnifiedID,
		cancel.ConjoinedPart,
		cancel.SegmentNum)

	if err != nil {
		return fmt.Errorf("cancel-segment %s", err)
	}

	return nil
}

// FinishSegment finishes storing the segment
func (writer *nimbusioWriter) FinishSegment(lgr logger.Logger,
	segment msg.Segment, file msg.File) error {
	var err error

	lgr.Debug("FinishSegment")

	key := segmentKey{segment.UnifiedID, segment.ConjoinedPart,
		segment.SegmentNum}
	entry, ok := writer.SegmentMap[key]
	if !ok {
		return fmt.Errorf("FinishSegment unknown segment %s", key)
	}

	delete(writer.SegmentMap, key)

	stmt := nodedb.Stmts["finish-segment"]
	_, err = stmt.Exec(
		file.FileSize,
		file.Adler32,
		file.MD5Digest,
		entry.SegmentID)

	if err != nil {
		return fmt.Errorf("finish-segment %s", err)
	}

	for _, metaEntry := range file.MetaData {
		stmt := nodedb.Stmts["new-meta-data"]
		_, err = stmt.Exec(
			segment.CollectionID,
			entry.SegmentID,
			metaEntry.Key,
			metaEntry.Value,
			segment.Timestamp)

		if err != nil {
			return fmt.Errorf("new-meta-data %s", err)
		}
	}

	return nil
}

// DestroyKey makes a key inaccessible
func (writer *nimbusioWriter) DestroyKey(lgr logger.Logger,
	segment msg.Segment,
	unifiedIDToDestroy uint64) error {

	var err error

	lgr.Debug("DestroyKey (%d)", unifiedIDToDestroy)

	if unifiedIDToDestroy > 0 {
		if segment.HandoffNodeID > 0 {
			stmt := nodedb.Stmts["new-tombstone-for-unified-id-for-handoff"]
			_, err = stmt.Exec(
				segment.CollectionID,
				segment.Key,
				segment.UnifiedID,
				segment.Timestamp,
				segment.SegmentNum,
				unifiedIDToDestroy,
				segment.SourceNodeID,
				segment.HandoffNodeID)

			if err != nil {
				return fmt.Errorf("new-tombstone-for-unified-id-for-handoff %d %s",
					unifiedIDToDestroy, err)
			}
		} else {
			stmt := nodedb.Stmts["new-tombstone-for-unified-id"]
			_, err = stmt.Exec(
				segment.CollectionID,
				segment.Key,
				segment.UnifiedID,
				segment.Timestamp,
				segment.SegmentNum,
				unifiedIDToDestroy,
				segment.SourceNodeID,
				segment.HandoffNodeID)

			if err != nil {
				return fmt.Errorf("new-tombstone-for-unified-id %d %s",
					unifiedIDToDestroy, err)
			}
		}

		stmt := nodedb.Stmts["delete-conjoined-for-unified-id"]
		_, err = stmt.Exec(
			segment.Timestamp,
			segment.CollectionID,
			segment.Key,
			unifiedIDToDestroy)

		if err != nil {
			return fmt.Errorf("delete-conjoined-for-unified-id %d %s",
				unifiedIDToDestroy, err)
		}
	} else {
		if segment.HandoffNodeID > 0 {
			stmt := nodedb.Stmts["new-tombstone-for-handoff"]
			_, err = stmt.Exec(
				segment.CollectionID,
				segment.Key,
				segment.UnifiedID,
				segment.Timestamp,
				segment.SegmentNum,
				segment.SourceNodeID,
				segment.HandoffNodeID)

			if err != nil {
				return fmt.Errorf("new-tombstone-for-handoff %s", err)
			}
		} else {
			stmt := nodedb.Stmts["new-tombstone"]
			_, err = stmt.Exec(
				segment.CollectionID,
				segment.Key,
				segment.UnifiedID,
				segment.Timestamp,
				segment.SegmentNum,
				segment.SourceNodeID)

			if err != nil {
				return fmt.Errorf("new-tombstone %s", err)
			}
		}

		stmt := nodedb.Stmts["delete-conjoined"]
		_, err = stmt.Exec(
			segment.Timestamp,
			segment.CollectionID,
			segment.Key,
			segment.UnifiedID)

		if err != nil {
			return fmt.Errorf("delete-conjoined %s", err)
		}
	}
	// Set delete_timestamp on all conjoined rows for this key
	// that are older than this tombstone

	return nil
}

// StartConjoinedArchive begins a conjoined archive
func (writer *nimbusioWriter) StartConjoinedArchive(lgr logger.Logger,
	conjoinedEntry types.ConjoinedEntry) error {
	var err error

	lgr.Debug("StartConjoinedArchive %s", conjoinedEntry)

	if conjoinedEntry.HandoffNodeID > 0 {
		stmt := nodedb.Stmts["start-conjoined-for-handoff"]
		_, err = stmt.Exec(
			conjoinedEntry.CollectionID,
			conjoinedEntry.Key,
			conjoinedEntry.UnifiedID,
			conjoinedEntry.Timestamp,
			conjoinedEntry.HandoffNodeID)

		if err != nil {
			return fmt.Errorf("start-conjoined-for-handoff %s", err)
		}
	} else {
		stmt := nodedb.Stmts["start-conjoined"]
		_, err = stmt.Exec(
			conjoinedEntry.CollectionID,
			conjoinedEntry.Key,
			conjoinedEntry.UnifiedID,
			conjoinedEntry.Timestamp)

		if err != nil {
			return fmt.Errorf("start-conjoined %s", err)
		}

	}

	return nil
}

// AbortConjoinedArchive cancels conjoined archive
func (writer *nimbusioWriter) AbortConjoinedArchive(lgr logger.Logger,
	conjoinedEntry types.ConjoinedEntry) error {
	var err error

	lgr.Debug("StartConjoinedArchive %s", conjoinedEntry)

	if conjoinedEntry.HandoffNodeID > 0 {

		stmt := nodedb.Stmts["abort-conjoined-for-handoff"]
		_, err = stmt.Exec(
			conjoinedEntry.Timestamp,
			conjoinedEntry.CollectionID,
			conjoinedEntry.Key,
			conjoinedEntry.UnifiedID,
			conjoinedEntry.HandoffNodeID)

		if err != nil {
			return fmt.Errorf("abort-conjoined-for-handoff %s", err)
		}
	} else {

		stmt := nodedb.Stmts["abort-conjoined"]
		_, err = stmt.Exec(
			conjoinedEntry.Timestamp,
			conjoinedEntry.CollectionID,
			conjoinedEntry.Key,
			conjoinedEntry.UnifiedID)

		if err != nil {
			return fmt.Errorf("abort-conjoined %s", err)
		}

	}

	return nil
}

// FinishConjoinedArchive completes a conjoined archive
func (writer *nimbusioWriter) FinishConjoinedArchive(lgr logger.Logger,
	conjoinedEntry types.ConjoinedEntry) error {
	var err error

	lgr.Debug("FinishConjoinedArchive %s", conjoinedEntry)

	if conjoinedEntry.HandoffNodeID > 0 {

		stmt := nodedb.Stmts["finish-conjoined-for-handoff"]
		_, err = stmt.Exec(
			conjoinedEntry.Timestamp,
			conjoinedEntry.CollectionID,
			conjoinedEntry.Key,
			conjoinedEntry.UnifiedID,
			conjoinedEntry.HandoffNodeID)

		if err != nil {
			return fmt.Errorf("finish-conjoined-for-handoff %s", err)
		}
	} else {

		stmt := nodedb.Stmts["finish-conjoined"]
		_, err = stmt.Exec(
			conjoinedEntry.Timestamp,
			conjoinedEntry.CollectionID,
			conjoinedEntry.Key,
			conjoinedEntry.UnifiedID)

		if err != nil {
			return fmt.Errorf("finish-conjoined %s", err)
		}

	}

	return nil
}
