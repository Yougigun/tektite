//lint:file-ignore U1000 Ignore all unused code
package levels

import (
	"bytes"
	"fmt"
	"github.com/google/uuid"
	"github.com/spirit-labs/tektite/common"
	"github.com/spirit-labs/tektite/conf"
	"github.com/spirit-labs/tektite/errors"
	log "github.com/spirit-labs/tektite/logger"
	"github.com/spirit-labs/tektite/objstore"
	"github.com/spirit-labs/tektite/sst"
	"github.com/spirit-labs/tektite/tabcache"
	"strings"
	"sync"
	"time"
)

/*
LevelManager
The basic idea is the manager knows, for a particular key range, which sstables contain keys in that range.
Tables are registered with the LevelManager after they have been pushed to a cloud store, and they are deregistered
by compaction when they are no longer needed.
We use the following data structure to store the mapping:
There are potentially many levels in the database, numbered from 0 to N.
Level 0 is a special level, and it's where newly created tables are initially pushed to. Level 0 is unique in that there
can be an overlap of keys between different sstables in that level.
For all other levels, keys in sstables do not overlap.
The master record maintains each level as a list of segmentEntries. Each segment has a key range and the id of the segment
which contains the actual sstable ids.
A segment is a structure that has a key range and a list of sstableids. A segment is stored as an object in the cloud store.
It is cached and retrieved lazily when required. At any one time we do not necessarily have all the segments that
represent the structure of the database in memory. This allows us to scale to very large database sizes - some levels
can have a large number of sstables.
Commands to mutate the level state are replicated across the cluster, and on failure will be reprocessed by the new
leader. The mechanism for this is the same as the mechanism used for standard SpiritDB replication.
Changed segments are periodically flushed to cloud storage.
*/
type LevelManager struct {
	lock                           sync.RWMutex
	flushLock                      sync.Mutex
	state                          levelManagerState
	format                         common.MetadataFormat
	objStore                       objstore.Client
	tabCache                       *tabcache.Cache
	commandBatchIngestor           commandBatchIngestor
	conf                           *conf.Config
	hasChanges                     bool
	segmentCache                   *segmentCache
	masterRecord                   *masterRecord
	segmentsToAdd                  map[string]*segment
	segmentsToDelete               map[string]struct{}
	masterRecordBufferSizeEstimate int
	segmentBufferSizeEstimate      int
	clusterVersions                map[string]int
	flushTimer                     *common.TimerHandle
	flushedCallback                func(err error)
	tablesToDelete                 []deleteTableEntry
	tableDeleteTimer               *common.TimerHandle
	enableCompaction               bool
	validateOnEachStateChange      bool
	inflightAdds                   int
	pendingAddsQueue               []pendingL0Add
	jobQueue                       []jobHolder
	inProgress                     map[string]inProgressCompaction
	pendingCompactions             map[int]int
	lockedTables                   map[string]struct{}
	pollers                        *pollerQueue
	stats                          CompactionStats
	removeDeadVersionsInProgress   bool
	enableDedup                    bool
}

type levelManagerState int

const (
	stateCreated  = levelManagerState(1)
	stateLoaded   = levelManagerState(2)
	stateActive   = levelManagerState(3)
	stateShutdown = levelManagerState(4)
	stateStopped  = levelManagerState(5)
)

type pendingL0Add struct {
	regBatch       RegistrationBatch
	completionFunc func(error)
}

type deleteTableEntry struct {
	tableID   sst.SSTableID
	addedTime uint64
}

const objStoreRetryInterval = 500 * time.Millisecond

func NewLevelManager(conf *conf.Config, cloudStore objstore.Client, tabCache *tabcache.Cache,
	commandBatchIngestor commandBatchIngestor, enableCompaction bool, validateOnEachStateChange bool, enableDedup bool) *LevelManager {
	lm := &LevelManager{
		format:                    conf.RegistryFormat,
		objStore:                  cloudStore,
		tabCache:                  tabCache,
		commandBatchIngestor:      commandBatchIngestor,
		conf:                      conf,
		segmentsToAdd:             map[string]*segment{},
		segmentsToDelete:          map[string]struct{}{},
		clusterVersions:           map[string]int{},
		segmentCache:              newSegmentCache(conf.SegmentCacheMaxSize),
		enableCompaction:          enableCompaction,
		validateOnEachStateChange: validateOnEachStateChange,
		pollers:                   &pollerQueue{},
		inProgress:                map[string]inProgressCompaction{},
		lockedTables:              map[string]struct{}{},
		pendingCompactions:        map[int]int{},
		enableDedup:               enableDedup,
		state:                     stateCreated,
	}
	return lm
}

func (lm *LevelManager) levelMaxTablesTrigger(level int) int {
	if level == 0 {
		return lm.conf.L0CompactionTrigger
	}
	mt := lm.conf.L1CompactionTrigger
	for i := 1; i < level; i++ {
		mt *= lm.conf.LevelMultiplier
	}
	return mt
}

func (lm *LevelManager) initialiseMasterRecord() (*masterRecord, error) {
	for {
		buff, err := lm.objStore.Get([]byte(lm.conf.MasterRegistryRecordID))
		if err != nil {
			if common.IsUnavailableError(err) {
				log.Warnf("object store is unavailable - will retry - %v", err)
				time.Sleep(objStoreRetryInterval)
				continue
			}
			return nil, errors.Errorf("levelManager failed to get master record from object store %v", err)
		}
		var mr *masterRecord
		if buff != nil {
			mr = &masterRecord{}
			mr.deserialize(buff, 0)
			log.Debugf("level manager initialised with last flushed version: %d %v", mr.lastFlushedVersion, mr)
		} else {
			mr = &masterRecord{
				format:               lm.conf.RegistryFormat,
				levelTableCounts:     map[int]int{},
				slabRetentions:       map[uint64]uint64{},
				lastFlushedVersion:   -1,
				lastProcessedReplSeq: -1,
				stats:                &Stats{LevelStats: map[int]*LevelStats{}},
			}
			buff := mr.serialize(nil)
			if err := lm.objStore.Put([]byte(lm.conf.MasterRegistryRecordID), buff); err != nil {
				if common.IsUnavailableError(err) {
					log.Warnf("object store is unavailable - will retry - %v", err)
					time.Sleep(objStoreRetryInterval)
					continue
				}
				return nil, errors.Errorf("levelManager failed to get master record from object store %v", err)
			}
			log.Debug("no master record found in store")
		}
		return mr, nil
	}
}

func (lm *LevelManager) Start(block bool) error {
	lm.lock.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			lm.lock.Unlock()
		}
	}()
	if lm.state != stateCreated {
		panic(fmt.Sprintf("invalid state:%d", lm.state))
	}
	ch := make(chan struct{}, 1)
	log.Debugf("level manager starting on node %d", lm.conf.NodeID)
	// We initialise on a separate goroutine as we don't want to block start in the case the obj store is not
	// available
	common.Go(func() {
		mr, err := lm.initialiseMasterRecord()
		if err != nil {
			log.Errorf("failed to initialise master record %v", err)
			return
		}
		lm.lock.Lock()
		defer lm.lock.Unlock()
		lm.masterRecord = mr
		if lm.conf.LevelManagerFlushInterval != -1 {
			// -1 disables periodic flushing (used in tests)
			lm.scheduleFlushNoLock(lm.conf.LevelManagerFlushInterval, true)
		}
		lm.scheduleTableDeleteTimer(true)
		lm.state = stateLoaded
		if len(lm.masterRecord.deadVersionRanges) > 0 {
			if err := lm.maybeScheduleRemoveDeadVersionEntries(); err != nil {
				log.Errorf("failed to schedule remove dead version entries on lmgr start: %v", err)
			}
		}
		log.Debugf("level manager loaded on node %d", lm.conf.NodeID)
		// Maybe trigger a compaction as levels could be full
		if err := lm.maybeScheduleCompaction(); err != nil {
			log.Errorf("failed to trigger compaction: %v", err)
		}
		ch <- struct{}{}
	})
	if block {
		lm.lock.Unlock()
		unlocked = true
		<-ch
	}
	return nil
}

func (lm *LevelManager) Stop() error {
	// We stop the timers without waiting for them to complete, then wait for them to complete outside the level manager
	// lock, to prevent deadlock where the timer GR tries to get the level manager lock
	var timers []*common.TimerHandle
	defer func() {
		for _, t := range timers {
			t.WaitComplete()
		}
	}()
	log.Debugf("level manager stopping on node %d", lm.conf.NodeID)

	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.flushTimer != nil {
		lm.flushTimer.Stop()
		timers = append(timers, lm.flushTimer)
	}
	if lm.tableDeleteTimer != nil {
		lm.tableDeleteTimer.Stop()
		timers = append(timers, lm.tableDeleteTimer)
	}
	for _, inProg := range lm.inProgress {
		if inProg.timer != nil {
			inProg.timer.Stop()
			timers = append(timers, inProg.timer)
		}
	}
	lm.state = stateStopped
	return nil
}

func (lm *LevelManager) Activate() error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state != stateLoaded {
		return errors.NewTektiteErrorf(errors.Unavailable, "levelManager not loaded")
	}
	lm.state = stateActive
	log.Debugf("level manager activated on node %d", lm.conf.NodeID)
	return nil
}

// Only used in testing
func (lm *LevelManager) reset() {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	lm.state = stateCreated
	lm.segmentsToAdd = map[string]*segment{}
	lm.segmentsToDelete = map[string]struct{}{}
	lm.clusterVersions = map[string]int{}
	lm.segmentCache = newSegmentCache(lm.conf.SegmentCacheMaxSize)
	lm.masterRecord = nil
}

func (lm *LevelManager) getClusterVersion(clusterName string) int {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return lm.clusterVersions[clusterName]
}

// Note that keyEnd is exclusive
func (lm *LevelManager) getOverlappingTables(keyStart []byte, keyEnd []byte, level int,
	segmentEntries []segmentEntry) ([]*TableEntry, error) {
	var tables []*TableEntry
	for _, segEntry := range segmentEntries {
		if hasOverlap(keyStart, keyEnd, segEntry.rangeStart, segEntry.rangeEnd) {
			seg, err := lm.getSegment(segEntry.segmentID)
			if err != nil {
				return nil, err
			}
			if seg == nil {
				panic("level manager segment not found")
			}
			if level == 0 {
				// We must add the overlapping entries from newest to oldest
				for i := len(seg.tableEntries) - 1; i >= 0; i-- {
					tabEntry := seg.tableEntries[i]
					if hasOverlap(keyStart, keyEnd, tabEntry.RangeStart, tabEntry.RangeEnd) {
						tables = append(tables, tabEntry)
					}
				}
			} else {
				// Other levels have no overlap, and there can be > 1 segment, so we just add them in the order they
				// appear
				for _, tabEntry := range seg.tableEntries {
					if hasOverlap(keyStart, keyEnd, tabEntry.RangeStart, tabEntry.RangeEnd) {
						tables = append(tables, tabEntry)
					}
				}
			}
		}
	}
	return tables, nil
}

func (lm *LevelManager) GetTableIDsForRange(keyStart []byte, keyEnd []byte) (OverlappingTableIDs, []VersionRange, error) {
	lm.lock.RLock()
	defer lm.lock.RUnlock()
	if lm.state != stateActive {
		return nil, nil, errors.NewTektiteErrorf(errors.Unavailable, "levelManager not active")
	}
	var overlapping OverlappingTableIDs
	for level, entries := range lm.masterRecord.levelSegmentEntries {
		tables, err := lm.getOverlappingTables(keyStart, keyEnd, level, entries.segmentEntries)
		if err != nil {
			return nil, nil, err
		}
		if level == 0 {
			// Level 0 is overlapping
			for _, table := range tables {
				overlapping = append(overlapping, []sst.SSTableID{table.SSTableID})
			}
		} else if tables != nil {
			// Other levels are non overlapping
			ssTableIDs := make([]sst.SSTableID, len(tables))
			for i := 0; i < len(tables); i++ {
				ssTableIDs[i] = tables[i].SSTableID
			}
			overlapping = append(overlapping, ssTableIDs)
		}
	}
	// And we return any dead version ranges, so the data for those versions can be filtered out
	deadRanges := make([]VersionRange, len(lm.masterRecord.deadVersionRanges))
	copy(deadRanges, lm.masterRecord.deadVersionRanges)
	return overlapping, deadRanges, nil
}

func (lm *LevelManager) RegisterL0Tables(registrationBatch RegistrationBatch, completionFunc func(error)) {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state != stateActive {
		completionFunc(errors.NewTektiteErrorf(errors.Unavailable, "levelManager not active"))
		return
	}
	if !(len(registrationBatch.DeRegistrations) == 0 && len(registrationBatch.Registrations) == 1) ||
		registrationBatch.Registrations[0].Level != 0 || registrationBatch.Compaction {
		completionFunc(errors.Errorf("not an L0 registration %v", registrationBatch))
		return
	}
	// we check cluster version - this protects against network partition where node is lost but is still running, new
	// node takes over, but old store tries to register tables after new node is active.
	lowestVersion := lm.clusterVersions[registrationBatch.ClusterName]
	if registrationBatch.ClusterVersion < lowestVersion {
		completionFunc(errors.NewTektiteErrorf(errors.Unavailable, "registration batch version is too low"))
		return
	}
	lm.clusterVersions[registrationBatch.ClusterName] = registrationBatch.ClusterVersion

	log.Debugf("in LevelManager RegisterL0Tables")
	if lm.getL0FreeSpace() >= 1 {
		lm.inflightAdds++
		log.Debugf("in LevelManager RegisterL0Tables - enough free space so applying now")
		lm.sendApplyChangesReliably(registrationBatch, completionFunc)
		return
	}
	// queue the request
	log.Debugf("in LevelManager RegisterL0Tables - not enough free space so queuing- %d", lm.getL0FreeSpace())

	lm.pendingAddsQueue = append(lm.pendingAddsQueue, pendingL0Add{
		regBatch:       registrationBatch,
		completionFunc: completionFunc,
	})
}

func (lm *LevelManager) sendApplyChangesReliably(regBatch RegistrationBatch, completionFunc func(error)) {
	// We send the ApplyChanges via the replication system, so it is reliable
	buff := make([]byte, 0, 256)
	buff = append(buff, ApplyChangesCommand)
	buff = regBatch.Serialize(buff)
	lm.commandBatchIngestor(buff, func(err error) {
		if err == nil {
			completionFunc(nil)
			return
		}
		lm.lock.Lock()
		lm.inflightAdds--
		lm.lock.Unlock()
		completionFunc(err)
	})
}

func (lm *LevelManager) getL0FreeSpace() int {
	return lm.conf.L0MaxTablesBeforeBlocking - lm.masterRecord.levelTableCounts[0] - lm.inflightAdds
}

func (lm *LevelManager) ApplyChangesNoCheck(registrationBatch RegistrationBatch) error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return lm.doApplyChanges(registrationBatch.Registrations, registrationBatch.DeRegistrations)
}

func (lm *LevelManager) checkDuplicate(replSeq int, reprocess bool) bool {
	if !lm.enableDedup || replSeq < -1 {
		// if replSeq is < -1 this means ignore deduplication, this is used by the no-op replicator
		return true
	}
	log.Debugf("checking replseq %d last %d reprocess %t", replSeq, lm.masterRecord.lastProcessedReplSeq, reprocess)
	ok := replSeq > lm.masterRecord.lastProcessedReplSeq
	if !ok {
		if !reprocess {
			// This can occur if the cluster wasn't shutdown properly, and restarts with replseq != 0, and then
			// receives a command with replSeq = 0. in this case we accept with warning
			log.Warn("duplicate level manager replSeq received. was the cluster shutdown properly before?")
			return true
		} else {
			log.Debugf("duplicate level manager command received on reprocessing - ignoring replSeq %d last %d", replSeq,
				lm.masterRecord.lastProcessedReplSeq)
		}
	}
	return ok
}

func (lm *LevelManager) updateReplSeq(replSeq int) {
	lm.masterRecord.lastProcessedReplSeq = replSeq
	lm.hasChanges = true
}

func (lm *LevelManager) checkStateForCommand(reprocess bool) error {
	if reprocess {
		// reprocess batches must be received after loaded and before active
		if lm.state != stateLoaded {
			return errors.NewTektiteErrorf(errors.Unavailable, fmt.Sprintf("invalid state:%d", lm.state))
		}
	} else {
		if lm.state != stateActive {
			return errors.NewTektiteErrorf(errors.Unavailable, fmt.Sprintf("invalid state:%d", lm.state))
		}
	}
	return nil
}

func (lm *LevelManager) ApplyChanges(registrationBatch RegistrationBatch, reprocess bool, replSeq int) error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if err := lm.checkStateForCommand(reprocess); err != nil {
		return err
	}
	log.Debugf("in levelmanager ApplyChanges. reprocess %t replseq %d", reprocess, replSeq)
	if !lm.checkDuplicate(replSeq, reprocess) {
		return nil
	}
	defer lm.updateReplSeq(replSeq)
	if registrationBatch.Compaction {
		return lm.applyCompactionChanges(registrationBatch, reprocess)
	}
	if !reprocess {
		lm.inflightAdds--
	}
	log.Debugf("in LevelManager ApplyChanges for L0")

	if err := lm.doApplyChanges(registrationBatch.Registrations, registrationBatch.DeRegistrations); err != nil {
		return err
	}

	log.Debugf("registered l0 table: %v now dumping, reprocess? %t", registrationBatch.Registrations[0].TableID, reprocess)
	if log.DebugEnabled {
		lm.dump()
	}
	if lm.enableCompaction {
		return lm.maybeScheduleCompaction()
	}
	return nil
}

func (lm *LevelManager) applyCompactionChanges(registrationBatch RegistrationBatch, reprocess bool) error {
	jobExists := lm.jobInProgress(registrationBatch.JobID)
	if !jobExists {
		// level manager might have failed over so job can't be found. In this case we don't apply the batch
		// as it might result in an inconsistent state as tables won't be locked and other jobs could be running
		// for same tables.
		// If this is reprocessing after failure we ignore as expected that job won't be found, this is OK
		if reprocess {
			return nil
		}
		return errors.NewTektiteErrorf(errors.CompactionJobNotFound, "job not found %s. possible level manager failover", registrationBatch.JobID)
	}
	if err := lm.doApplyChanges(registrationBatch.Registrations, registrationBatch.DeRegistrations); err != nil {
		return err
	}

	registeredTables := make(map[string]struct{}, len(registrationBatch.Registrations))
	for _, registration := range registrationBatch.Registrations {
		registeredTables[string(registration.TableID)] = struct{}{}
	}
	tablesToDelete := make([]deleteTableEntry, 0, len(registrationBatch.DeRegistrations))
	now := common.NanoTime()
	// For each deRegistration we add the table id to the tables to delete UNLESS the same table has also been
	// registered in the batch - this can happen when a table is moved from one level to the next - we do not want to
	// delete it then.
	l0DeRegs := 0
	for _, deRegistration := range registrationBatch.DeRegistrations {
		if deRegistration.Level == 0 {
			l0DeRegs++
		}
		_, registered := registeredTables[string(deRegistration.TableID)]
		if !registered {
			tablesToDelete = append(tablesToDelete, deleteTableEntry{
				tableID:   deRegistration.TableID,
				addedTime: now,
			})
		}
	}

	// ss-tables are deleted after a delay - this allows any queries currently in execution some time
	lm.tablesToDelete = append(lm.tablesToDelete, tablesToDelete...)
	if !reprocess {
		if err := lm.compactionComplete(registrationBatch.JobID); err != nil {
			return err
		}
		lm.maybeDespatchPendingL0Adds()
	} else {
		log.Debugf("compaction complete but reprocess so not checking pending adds")
	}
	return nil
}

func (lm *LevelManager) maybeDespatchPendingL0Adds() {
	freeSpace := lm.getL0FreeSpace()
	log.Debugf("in levelmanager maybeDespatchPendingL0Adds, freespace is %d", freeSpace)
	if freeSpace <= 0 {
		return
	}
	toDespatch := freeSpace
	if len(lm.pendingAddsQueue) < toDespatch {
		toDespatch = len(lm.pendingAddsQueue)
	}
	log.Debugf("sending %d queueing l0 add", toDespatch)
	for i := 0; i < toDespatch; i++ {
		pending := lm.pendingAddsQueue[i]
		lm.inflightAdds++
		lm.sendApplyChangesReliably(pending.regBatch, pending.completionFunc)
	}
	lm.pendingAddsQueue = lm.pendingAddsQueue[toDespatch:]
}

func (lm *LevelManager) MaybeScheduleCompaction() error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state != stateLoaded && lm.state != stateActive {
		return nil
	}
	return lm.maybeScheduleCompaction()
}

func (lm *LevelManager) getLastLevel() int {
	return len(lm.masterRecord.levelSegmentEntries) - 1
}

// RegisterDeadVersionRange - registers a range of versions as dead - versions in the dead range will be removed from
// the store via compaction, asynchronously. Note the version range is inclusive.
func (lm *LevelManager) RegisterDeadVersionRange(versionRange VersionRange, clusterName string, clusterVersion int,
	reprocess bool, replSeq int) error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	log.Debugf("in levelmanager RegisterDeadVersionRange. replseq %d", replSeq)
	if err := lm.checkStateForCommand(reprocess); err != nil {
		return err
	}
	if !lm.checkDuplicate(replSeq, reprocess) {
		return nil
	}
	defer lm.updateReplSeq(replSeq)
	lowestVersion := lm.clusterVersions[clusterName]
	if clusterVersion < lowestVersion {
		return errors.NewTektiteErrorf(errors.Unavailable,
			"RegisterDeadVersionRange - registration batch version is too low %d expected %d", clusterVersion, lowestVersion)
	}
	// We update the cluster version - this prevents L0 tables with a dead version range being pushed after this has been
	// called - as we clear the local store when we get the new cluster version in proc mgr.
	log.Debugf("registering dead version %v range with clusterVersion %d", versionRange, clusterVersion)
	// sanity check
	alreadyExists := false
	for _, vr := range lm.masterRecord.deadVersionRanges {
		if versionRange.VersionStart == vr.VersionStart && versionRange.VersionEnd == vr.VersionEnd {
			alreadyExists = true
		}
	}
	if alreadyExists {
		// do nothing - client could have resent
		log.Debugf("duplicate RegisterDeadVersionRange call received - will be ignored")
		return nil
	}
	lm.clusterVersions[clusterName] = clusterVersion
	lm.masterRecord.deadVersionRanges = append(lm.masterRecord.deadVersionRanges, versionRange)
	lm.hasChanges = true
	// We will try and prompt a compaction to remove entries for this version range
	if err := lm.maybeScheduleRemoveDeadVersionEntries(); err != nil {
		return err
	}
	return nil
}

// LevelIterator - only used in testing
func (lm *LevelManager) LevelIterator(level int) (LevelIterator, error) {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return lm.levelIterator(level)
}

func (lm *LevelManager) levelIterator(level int) (LevelIterator, error) {
	entries := lm.getLevelSegmentEntries(level)
	return newLevelIter(lm, entries.segmentEntries)
}

type levelIter struct {
	lm          *LevelManager
	segEntries  []segmentEntry
	segEntryPos int
	currSegment *segment
	segPos      int
}

func newLevelIter(levelManager *LevelManager, segEntries []segmentEntry) (*levelIter, error) {
	li := &levelIter{
		lm:         levelManager,
		segEntries: segEntries,
	}
	if err := li.Reset(); err != nil {
		return nil, err
	}
	return li, nil
}

func (l *levelIter) Reset() error {
	if len(l.segEntries) == 0 {
		return nil
	}
	var err error
	l.currSegment, err = l.lm.getSegment(l.segEntries[0].segmentID)
	l.segPos = -1
	return err
}

func (l *levelIter) Next() (*TableEntry, error) {
	if l.currSegment == nil {
		return nil, nil
	}
	if l.segPos == len(l.currSegment.tableEntries)-1 {
		if l.segEntryPos == len(l.segEntries)-1 {
			// No more
			return nil, nil
		}
		// Go to next segment
		l.segEntryPos++
		l.segPos = 0
		var err error
		l.currSegment, err = l.lm.getSegment(l.segEntries[l.segEntryPos].segmentID)
		if err != nil {
			return nil, err
		}
	} else {
		l.segPos++
	}
	entry := l.currSegment.tableEntries[l.segPos]
	return entry, nil
}

func (lm *LevelManager) doApplyChanges(registrations []RegistrationEntry, deRegistrations []RegistrationEntry) error {

	if log.DebugEnabled {
		var dsb strings.Builder
		for _, dereg := range deRegistrations {
			dsb.WriteString(fmt.Sprintf("%v", []byte(dereg.TableID)))
			dsb.WriteRune(',')
		}
		var rsb strings.Builder
		for _, reg := range registrations {
			rsb.WriteString(fmt.Sprintf("%v", []byte(reg.TableID)))
			rsb.WriteRune(',')
		}
		log.Debugf("applychanges: deregistering: %s registering: %s", dsb.String(), rsb.String())
	}

	// We make a copy of the state that is mutated when we apply changes, so we can rollback if an error occurs
	mrCopy := lm.masterRecord.copy()
	segsToAddCopy := make(map[string]*segment, len(lm.segmentsToAdd))
	for sid, seg := range lm.segmentsToAdd {
		segsToAddCopy[sid] = seg
	}
	segsToDeleteCopy := make(map[string]struct{}, len(lm.segmentsToDelete))
	for sid, struc := range lm.segmentsToDelete {
		segsToDeleteCopy[sid] = struc
	}

	// We must process the de-registrations before registrations, or we can temporarily have overlapping keys
	err := lm.applyDeRegistrations(deRegistrations)
	if err == nil {
		err = lm.applyRegistrations(registrations)
	}
	if err != nil {
		// Rollback to the previous state
		lm.masterRecord = mrCopy
		lm.segmentsToAdd = segsToAddCopy
		lm.segmentsToDelete = segsToDeleteCopy
		return err
	}

	if lm.validateOnEachStateChange {
		if err := lm.Validate(false); err != nil {
			return err
		}
	}

	lm.masterRecord.version++
	lm.hasChanges = true
	return nil
}

func (lm *LevelManager) applyDeRegistrations(deRegistrations []RegistrationEntry) error { //nolint:gocyclo
	for _, deRegistration := range deRegistrations {
		if len(deRegistration.KeyStart) == 0 || len(deRegistration.KeyEnd) <= 8 {
			return errors.Errorf("deregistration, key start/end does not have a version: %v", deRegistration)
		}
		entries := lm.getLevelSegmentEntries(deRegistration.Level)
		segmentEntries := entries.segmentEntries
		if len(segmentEntries) == 0 {
			// Can occur if deRegistration applied more than once - we need to be idempotent
			continue
		}
		var segEntry *segmentEntry
		found := -1
		if deRegistration.Level == 0 {
			if len(segmentEntries) == 0 {
				return errors.Error("no segment for level 0")
			}
			segEntry = &segmentEntries[0]
			found = 0
		} else {
			// Find which segment entry the table is in
			for i, entry := range segmentEntries {
				if bytes.Compare(deRegistration.KeyStart, entry.rangeStart) >= 0 && bytes.Compare(deRegistration.KeyEnd, entry.rangeEnd) <= 0 {
					found = i
					break
				}
			}
			if found == -1 {
				// This can occur if deRegistration is applied more than once - we are idempotent.
				// E.g. during reprocessing or if the client resubmits after a network error but it had actually been
				// applied already
				continue
			}
			segEntry = &segmentEntries[found]
		}

		// Load the segment
		seg, err := lm.getSegment(segEntry.segmentID)
		if err != nil {
			return err
		}

		if seg == nil {
			// This can occur if deRegistration is applied more than once - we are idempotent.
			// E.g. during reprocessing or if the client resubmits after a network error but it had actually been
			// applied already
			continue
		}

		// Find the table entry in the segment entry
		pos := -1
		for i, te := range seg.tableEntries {
			if bytes.Equal(deRegistration.TableID, te.SSTableID) {
				pos = i
				break
			}
		}
		if pos == -1 {
			// This can occur if deRegistration is applied more than once - we are idempotent.
			// E.g. during reprocessing or if the client resubmits after a network error but it had actually been
			// applied already
			continue
		}
		newTableEntries := make([]*TableEntry, pos)
		copy(newTableEntries, seg.tableEntries[:pos])

		newTableEntries = append(newTableEntries, seg.tableEntries[pos+1:]...)

		// Remove the old segment
		lm.segmentToRemove(segEntry.segmentID)

		if len(newTableEntries) == 0 {
			// We remove the segment entry - it is empty
			newSegEntries := segmentEntries[:found]
			newSegEntries = append(newSegEntries, segmentEntries[found+1:]...)
			segmentEntries = newSegEntries
		} else {
			newSeg := &segment{
				format:       seg.format,
				tableEntries: newTableEntries,
			}
			// Add the new segment
			id, err := lm.segmentToAdd(newSeg)
			if err != nil {
				return err
			}
			var newStart, newEnd []byte
			if deRegistration.Level == 0 {
				// Level 0 is not ordered so we need to scan through all of them
				for _, te := range newTableEntries {
					if newStart == nil || bytes.Compare(te.RangeStart, newStart) < 0 {
						newStart = te.RangeStart
					}
					if newEnd == nil || bytes.Compare(te.RangeEnd, newEnd) > 0 {
						newEnd = te.RangeEnd
					}
				}
			} else {
				newStart = newTableEntries[0].RangeStart
				newEnd = newTableEntries[len(newTableEntries)-1].RangeEnd
			}
			newSegEntry := segmentEntry{
				format:     segEntry.format,
				segmentID:  id,
				rangeStart: newStart,
				rangeEnd:   newEnd,
			}
			segmentEntries[found] = newSegEntry
		}
		entries = levelEntries{
			segmentEntries: segmentEntries,
			maxVersion:     entries.maxVersion,
		}
		lm.setLevelSegmentEntries(deRegistration.Level, entries)
		lm.masterRecord.levelTableCounts[deRegistration.Level]--

		lm.masterRecord.stats.TotTables--
		lm.masterRecord.stats.TotBytes -= int(deRegistration.TableSize)
		lm.masterRecord.stats.TotEntries -= int(deRegistration.NumEntries)
		levStats := lm.getLevelStats(deRegistration.Level)
		levStats.Tables--
		levStats.Bytes -= int(deRegistration.TableSize)
		levStats.Entries -= int(deRegistration.NumEntries)
	}
	return nil
}

func (lm *LevelManager) getLevelSegmentEntries(level int) levelEntries {
	lm.maybeResizeLevelSegmentEntries(level)
	return lm.masterRecord.levelSegmentEntries[level]
}

func (lm *LevelManager) setLevelSegmentEntries(level int, entries levelEntries) {
	lm.maybeResizeLevelSegmentEntries(level)
	lm.masterRecord.levelSegmentEntries[level] = entries
}

func (lm *LevelManager) maybeResizeLevelSegmentEntries(level int) {
	if level >= len(lm.masterRecord.levelSegmentEntries) {
		newEntries := make([]levelEntries, level+1)
		copy(newEntries, lm.masterRecord.levelSegmentEntries)
		lm.masterRecord.levelSegmentEntries = newEntries
	}
}

func (lm *LevelManager) applyRegistrations(registrations []RegistrationEntry) error { //nolint:gocyclo
	for _, registration := range registrations {

		if len(registration.KeyStart) == 0 || len(registration.KeyEnd) <= 8 {
			return errors.Errorf("registration, key start/end does not have a version: %v", registration)
		}

		log.Debugf("LevelManager registering new table %v (%s) from %s to %s in level %d",
			registration.TableID, string(registration.TableID), string(registration.KeyStart), string(registration.KeyEnd), registration.Level)

		// The new table entry that we're going to add
		tabEntry := &TableEntry{
			SSTableID:        registration.TableID,
			RangeStart:       registration.KeyStart,
			RangeEnd:         registration.KeyEnd,
			MinVersion:       registration.MinVersion,
			MaxVersion:       registration.MaxVersion,
			DeleteRatio:      registration.DeleteRatio,
			AddedTime:        registration.AddedTime,
			NumEntries:       registration.NumEntries,
			Size:             registration.TableSize,
			NumPrefixDeletes: registration.NumPrefixDeletes,
		}
		entries := lm.getLevelSegmentEntries(registration.Level)
		segmentEntries := entries.segmentEntries
		maxVersion := entries.maxVersion
		if registration.MaxVersion > maxVersion {
			maxVersion = registration.MaxVersion
		}
		if registration.Level == 0 {
			// We have overlapping keys in L0, so we just append to the last segment
			var seg *segment
			var segRangeStart, segRangeEnd []byte

			if len(segmentEntries) > 0 {
				// Segment already exists
				// Level 0 only ever has one segment
				l0SegmentEntry := segmentEntries[0]
				segCurr, err := lm.getSegment(l0SegmentEntry.segmentID)
				if err != nil {
					return err
				}
				if containsTable(segCurr, registration.TableID) {
					// Already added - this con occur on recovery or if client resubmits request after
					// we are idempotent
					continue
				}
				// make a copy
				copiedEntries := make([]*TableEntry, len(segCurr.tableEntries))
				copy(copiedEntries, segCurr.tableEntries)
				seg = &segment{
					format:       segCurr.format,
					tableEntries: copiedEntries,
				}
				seg.tableEntries = append(seg.tableEntries, tabEntry)
				segRangeStart = l0SegmentEntry.rangeStart
				segRangeEnd = l0SegmentEntry.rangeEnd
				// Update the ranges
				if bytes.Compare(registration.KeyStart, segRangeStart) < 0 {
					segRangeStart = registration.KeyStart
				}
				if bytes.Compare(registration.KeyEnd, segRangeEnd) > 0 {
					segRangeEnd = registration.KeyEnd
				}
				// Delete the old segment
				lm.segmentToRemove(l0SegmentEntry.segmentID)
			} else {
				// Create a new segment
				seg = &segment{
					tableEntries: []*TableEntry{tabEntry},
				}
				segRangeStart = registration.KeyStart
				segRangeEnd = registration.KeyEnd
			}

			// Add the new segment
			id, err := lm.segmentToAdd(seg)
			if err != nil {
				return err
			}

			// Update the master record
			log.Debugf("updating master record with segment id %v", id)
			entries := levelEntries{
				segmentEntries: []segmentEntry{{
					segmentID:  id,
					rangeStart: segRangeStart,
					rangeEnd:   segRangeEnd,
				}},
				maxVersion: maxVersion,
			}

			lm.setLevelSegmentEntries(0, entries)
		} else {

			// L > 0
			// Segments in these levels are non overlapping

			// Find which segment the new registration belongs in
			found := -1
			for i := 0; i < len(segmentEntries); i++ {
				// If the new table key start is after the key end of the previous segment (or there is no previous segment)
				// and the new table key end is before the key start of the next segment (or there is no next segment)
				// then we add the table entry to the current segment
				if (i == 0 || bytes.Compare(registration.KeyStart, segmentEntries[i-1].rangeEnd) > 0) &&
					(i == len(segmentEntries)-1 || bytes.Compare(registration.KeyEnd, segmentEntries[i+1].rangeStart) < 0) {
					found = i
					break
				}
			}
			if len(segmentEntries) > 0 && found == -1 {
				panic("cannot find segment for new table entry")
			}
			if found != -1 {
				segEntry := segmentEntries[found]
				seg, err := lm.getSegment(segEntry.segmentID)
				if err != nil {
					return err
				}
				if containsTable(seg, registration.TableID) {
					// Already added - this con occur on recovery or if client resubmits request after a previous
					// failure. Note dedup detection alone will not deal with this as the client can submit, so
					// we need to deal with it explicitly
					continue
				}
				// Find the insert before point
				insertPoint := -1
				for i, te := range seg.tableEntries {
					if bytes.Compare(registration.KeyEnd, te.RangeStart) < 0 {
						insertPoint = i
						break
					}
				}

				if insertPoint > 0 {
					// check no overlap with previous table entry
					l := seg.tableEntries[insertPoint-1]
					if bytes.Compare(l.RangeEnd, registration.KeyStart) >= 0 {
						msg := fmt.Sprintf("got overlap with previous table id %s, prev key end %s inserting key start %s inserting key end %s",
							string(l.SSTableID), string(l.RangeEnd), string(registration.KeyStart),
							string(registration.KeyEnd))
						return errors.Error(msg)
					}
				}

				// Insert the new entry in the table entries in the right place
				var newTableEntries []*TableEntry
				if insertPoint >= 0 {
					left := seg.tableEntries[:insertPoint]
					right := seg.tableEntries[insertPoint:]
					newTableEntries = append(newTableEntries, left...)
					newTableEntries = append(newTableEntries, tabEntry)
					newTableEntries = append(newTableEntries, right...)
				} else if insertPoint == -1 {
					newTableEntries = append(newTableEntries, seg.tableEntries...)
					newTableEntries = append(newTableEntries, tabEntry)
				}

				var nextSegID segmentID
				// Create the new segment(s)
				var newSegs []segment
				lnte := len(newTableEntries)
				if lnte > lm.conf.MaxRegistrySegmentTableEntries {
					// Too many entries
					// If there is a next segment, and it's not full we will merge it into that one otherwise
					// we will create a new segment
					merged := false
					if found < len(segmentEntries)-1 {
						nextSegID = segmentEntries[found+1].segmentID
						nextSeg, err := lm.getSegment(nextSegID)
						if err != nil {
							return err
						}
						if len(nextSeg.tableEntries) < lm.conf.MaxRegistrySegmentTableEntries {
							te1 := newTableEntries[:lnte-1]
							te2 := make([]*TableEntry, 0, len(nextSeg.tableEntries)+1)
							te2 = append(te2, newTableEntries[lnte-1])
							te2 = append(te2, nextSeg.tableEntries...)
							newSegs = append(newSegs, segment{format: byte(lm.format), tableEntries: te1}, segment{format: byte(lm.format), tableEntries: te2})
							merged = true
						} else {
							nextSegID = nil
						}
					}
					if !merged {
						// We didn't merge into the next one, so create a new segment
						te1 := newTableEntries[:lnte-1]
						te2 := newTableEntries[lnte-1:]
						newSegs = append(newSegs, segment{format: byte(lm.format), tableEntries: te1}, segment{format: byte(lm.format), tableEntries: te2})
					}
				} else {
					newSegs = []segment{{format: byte(lm.format), tableEntries: newTableEntries}}
				}

				// Delete the old segment
				lm.segmentToRemove(segEntry.segmentID)
				// Delete the next segment if we replaced that too
				if nextSegID != nil {
					lm.segmentToRemove(nextSegID)
				}
				// Client the new segment(s)
				newEntries := make([]segmentEntry, len(newSegs))
				for i, newSeg := range newSegs {
					nseg := newSeg
					id, err := lm.segmentToAdd(&nseg)
					if err != nil {
						return err
					}
					newEntries[i] = segmentEntry{
						segmentID:  id,
						rangeStart: newSeg.tableEntries[0].RangeStart,
						rangeEnd:   newSeg.tableEntries[len(newSeg.tableEntries)-1].RangeEnd,
					}
				}

				// Create the new segment entries
				newSegEntries := make([]segmentEntry, 0, len(segmentEntries)-1+len(newSegs))
				newSegEntries = append(newSegEntries, segmentEntries[:found]...)
				newSegEntries = append(newSegEntries, newEntries...)
				pos := found + 1
				if nextSegID != nil {
					// We changed the next segment too, so we replace two entries
					pos++
				}
				newSegEntries = append(newSegEntries, segmentEntries[pos:]...)
				entries := levelEntries{
					segmentEntries: newSegEntries,
					maxVersion:     maxVersion,
				}
				// Update the master record
				lm.setLevelSegmentEntries(registration.Level, entries)
			} else {
				// The first segment in the level
				seg := &segment{tableEntries: []*TableEntry{tabEntry}}
				id, err := lm.segmentToAdd(seg)
				if err != nil {
					return err
				}
				segEntry := segmentEntry{
					segmentID:  id,
					rangeStart: registration.KeyStart,
					rangeEnd:   registration.KeyEnd,
				}
				entries := levelEntries{
					segmentEntries: []segmentEntry{segEntry},
					maxVersion:     maxVersion,
				}
				lm.setLevelSegmentEntries(registration.Level, entries)
			}
		}
		lm.masterRecord.levelTableCounts[registration.Level]++

		if registration.Level == 0 {
			lm.masterRecord.stats.TablesIn++
			lm.masterRecord.stats.BytesIn += int(registration.TableSize)
			lm.masterRecord.stats.EntriesIn += int(registration.NumEntries)
		}
		lm.masterRecord.stats.TotTables++
		lm.masterRecord.stats.TotBytes += int(registration.TableSize)
		lm.masterRecord.stats.TotEntries += int(registration.NumEntries)
		levStats := lm.getLevelStats(registration.Level)
		levStats.Tables++
		levStats.Bytes += int(registration.TableSize)
		levStats.Entries += int(registration.NumEntries)
	}
	return nil
}

func (lm *LevelManager) getLevelStats(level int) *LevelStats {
	levStats, ok := lm.masterRecord.stats.LevelStats[level]
	if !ok {
		levStats = &LevelStats{}
		lm.masterRecord.stats.LevelStats[level] = levStats
	}
	return levStats
}

func (lm *LevelManager) getSegment(segmentID []byte) (*segment, error) {
	skey := common.ByteSliceToStringZeroCopy(segmentID)
	seg := lm.segmentCache.get(skey)
	if seg != nil {
		return seg, nil
	}
	buff, err := lm.objStore.Get(segmentID)
	if err != nil {
		return nil, err
	}
	if buff == nil {
		return nil, nil
	}
	segment := &segment{}
	segment.deserialize(buff)
	lm.segmentCache.put(skey, segment)
	return segment, nil
}

func (lm *LevelManager) getMasterRecord() *masterRecord {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return lm.masterRecord.copy()
}

func (lm *LevelManager) segmentToAdd(seg *segment) ([]byte, error) {
	sid := fmt.Sprintf("lmgr-seg-%s", uuid.New().String())
	lm.segmentCache.put(sid, seg)
	lm.segmentsToAdd[sid] = seg
	log.Debugf("LevelManager added segment with id %s to segmentsToAdd", sid)
	return []byte(sid), nil
}

func (lm *LevelManager) segmentToRemove(segID segmentID) {
	sid := common.ByteSliceToStringZeroCopy(segID)
	lm.segmentCache.delete(sid)
	if _, exists := lm.segmentsToAdd[sid]; exists {
		// The seg was created after last Flush so just delete it from segmentsToAdd
		log.Debugf("LevelManager deleting with id %v from segmentsToAdd", segID)
		delete(lm.segmentsToAdd, sid)
	} else {
		log.Debugf("LevelManager adding segment with id %v to segmentsToDelete", segID)
		lm.segmentsToDelete[sid] = struct{}{}
	}
}

func (lm *LevelManager) scheduleFlush(delay time.Duration, first bool) {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state == stateShutdown || lm.state == stateStopped {
		return
	}
	lm.scheduleFlushNoLock(delay, first)
}

func (lm *LevelManager) scheduleFlushNoLock(delay time.Duration, first bool) {
	lm.flushTimer = common.ScheduleTimer(delay, first, func() {
		_, _, err := lm.Flush(false)
		if err != nil {
			if common.IsUnavailableError(err) {
				log.Warnf("LevelManager unavailable to flush. will retry. %v", err)
				// schedule a flush sooner
				lm.scheduleFlush(1*time.Second, false)
			}
			log.Errorf("LevelManager failed to Flush %+v", err)
			return
		}
		lm.scheduleFlush(lm.conf.LevelManagerFlushInterval, false)
	})
}

func (lm *LevelManager) scheduleTableDeleteTimer(first bool) {
	lm.tableDeleteTimer = common.ScheduleTimer(lm.conf.SSTableDeleteCheckInterval, first, func() {
		lm.maybeDeleteTables()
	})
}

func (lm *LevelManager) maybeDeleteTables() {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state == stateShutdown || lm.state == stateStopped {
		return
	}
	pos := -1
	now := common.NanoTime()
	for i, entry := range lm.tablesToDelete {
		age := time.Duration(now - entry.addedTime)
		if age < lm.conf.SSTableDeleteDelay {
			break
		}
		log.Debugf("deleted sstable %v", entry.tableID)
		if err := lm.objStore.Delete(entry.tableID); err != nil {
			log.Errorf("failed to delete ss-table from cloud store: %v", err)
			break
		}
		lm.tabCache.DeleteSSTable(entry.tableID)
		pos = i
	}
	if pos != -1 {
		lm.tablesToDelete = lm.tablesToDelete[pos+1:]
	}
	lm.scheduleTableDeleteTimer(false)
}

func (lm *LevelManager) AddFlushedCallback(callback func(err error)) {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state == stateShutdown || lm.state == stateStopped {
		return
	}
	lm.flushedCallback = callback
}

func (lm *LevelManager) Flush(shutdown bool) (int, int, error) {
	// flushLock prevents Flush being called concurrently (e.g. by shutdown and timer) which could result in
	// callback being called twice.
	lm.flushLock.Lock()
	defer lm.flushLock.Unlock()
	lm.lock.Lock()

	if shutdown {
		// reset the replseq - on restart of the cluster all batch repl seqs in replicators will restart with zero.
		lm.masterRecord.lastProcessedReplSeq = -1
		lm.hasChanges = true
		lm.state = stateShutdown
	}

	if lm.state == stateStopped || !lm.hasChanges {
		lm.lock.Unlock()
		return 0, 0, nil
	}
	masterRecordToFlush := lm.masterRecord.copy()
	// Make copies of segments to add and delete
	segsToAdd := make(map[string]*segment, len(lm.segmentsToAdd))
	for id, seg := range lm.segmentsToAdd {
		segsToAdd[id] = seg
	}
	segsToDelete := make(map[string]struct{}, len(lm.segmentsToDelete))
	for id, s := range lm.segmentsToDelete {
		segsToDelete[id] = s
	}

	lm.segmentsToAdd = map[string]*segment{}
	lm.segmentsToDelete = map[string]struct{}{}
	lm.hasChanges = false
	flushedCallback := lm.flushedCallback
	lm.lock.Unlock()

	// We push outside the lock as it's relatively slow, and we don't want to prevent queries being executed
	segsAdded, segsDeleted, err := lm.pushSegmentsAndMasterRecord(masterRecordToFlush, segsToAdd, segsToDelete)
	if err != nil {
		if common.IsUnavailableError(err) {
			lm.lock.Lock()
			// Put the segs to add and delete back, so they will be retried next time
			for id, seg := range segsToAdd {
				lm.segmentsToAdd[id] = seg
			}
			for id, s := range segsToDelete {
				lm.segmentsToDelete[id] = s
			}
			lm.hasChanges = true
			lm.lock.Unlock()
		}
		return 0, 0, err
	}

	lm.lock.Lock()
	lm.segmentCache.flush()
	// flushed callback only gets called once - we defer setting it to nil to the end in case an error occurs, where
	// we want to retry
	lm.flushedCallback = nil
	lastFlushed := lm.masterRecord.lastFlushedVersion
	lastProcessed := lm.masterRecord.lastProcessedReplSeq
	lm.lock.Unlock()
	if flushedCallback != nil { // must be called outside lock to avoid deadlock with proc mgr
		flushedCallback(nil)
	}
	log.Debugf("levelManager flush complete, last flushed version %d replseq is %d", lastFlushed, lastProcessed)
	return segsAdded, segsDeleted, nil
}

func (lm *LevelManager) pushSegmentsAndMasterRecord(masterRecordToFlush *masterRecord, segsToAdd map[string]*segment,
	segsToDelete map[string]struct{}) (int, int, error) {

	// First delete segments
	segsDeleted := len(segsToDelete)
	for sid := range segsToDelete {
		segID := common.StringToByteSliceZeroCopy(sid)
		if err := lm.objStore.Delete(segID); err != nil {
			return 0, 0, err
		}
		log.Debugf("LevelManager deleted segment %v from cloud store", common.StringToByteSliceZeroCopy(sid))
	}
	// Then the adds
	segsAdded := len(segsToAdd)
	for sid, seg := range segsToAdd {
		segID := common.StringToByteSliceZeroCopy(sid)
		buff := make([]byte, 0, lm.segmentBufferSizeEstimate)
		buff = seg.serialize(buff)
		lm.updateSegmentBufferSizeEstimate(len(buff))
		if err := lm.objStore.Put(segID, buff); err != nil {
			return 0, 0, err
		}
		log.Debugf("LevelManager added segment %v to cloud store", common.StringToByteSliceZeroCopy(sid))
	}
	// Once they've all been added we can Flush the master record
	buff := make([]byte, 0, lm.masterRecordBufferSizeEstimate)
	buff = masterRecordToFlush.serialize(buff)
	lm.updateMasterRecordBufferSizeEstimate(len(buff))
	err := lm.objStore.Put([]byte(lm.conf.MasterRegistryRecordID), buff)
	if err != nil {
		return 0, 0, err
	}
	log.Debugf("LevelManager flushed masterrecord version %d", masterRecordToFlush.version)
	return segsAdded, segsDeleted, nil
}

func hasOverlap(keyStart []byte, keyEnd []byte, blockKeyStart []byte, blockKeyEnd []byte) bool {
	// Note! keyStart is inclusive, keyEnd is exclusive
	// LevelManager keyStart and keyEnd are inclusive!
	dontOverlapRight := bytes.Compare(keyStart, blockKeyEnd) > 0                  // Range starts after end of block
	dontOverlapLeft := keyEnd != nil && bytes.Compare(keyEnd, blockKeyStart) <= 0 // Range ends before beginning of block
	dontOverlap := dontOverlapLeft || dontOverlapRight
	return !dontOverlap
}

func (lm *LevelManager) GetSlabRetention(slabID int) (time.Duration, error) {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state != stateActive {
		return 0, errors.NewTektiteErrorf(errors.Unavailable, "levelManager not active")
	}
	ret := time.Duration(lm.masterRecord.slabRetentions[uint64(slabID)])
	return ret, nil
}

func (lm *LevelManager) RegisterSlabRetention(slabID int, retention time.Duration, reprocess bool, replSeq int) error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if err := lm.checkStateForCommand(reprocess); err != nil {
		return err
	}
	if !lm.checkDuplicate(replSeq, reprocess) {
		return nil
	}
	defer lm.updateReplSeq(replSeq)
	lm.masterRecord.slabRetentions[uint64(slabID)] = uint64(retention)
	lm.masterRecord.version++
	lm.hasChanges = true
	return nil
}

func (lm *LevelManager) UnregisterSlabRetention(slabID int, reprocess bool, replSeq int) error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if err := lm.checkStateForCommand(reprocess); err != nil {
		return err
	}
	if !lm.checkDuplicate(replSeq, reprocess) {
		return nil
	}
	defer lm.updateReplSeq(replSeq)
	delete(lm.masterRecord.slabRetentions, uint64(slabID))
	lm.masterRecord.version++
	lm.hasChanges = true
	return nil
}

func (lm *LevelManager) updateMasterRecordBufferSizeEstimate(buffSize int) {
	if buffSize > lm.masterRecordBufferSizeEstimate {
		lm.masterRecordBufferSizeEstimate = int(float64(buffSize) * 1.05)
	}
}

func (lm *LevelManager) updateSegmentBufferSizeEstimate(buffSize int) {
	if buffSize > lm.segmentBufferSizeEstimate {
		lm.segmentBufferSizeEstimate = int(float64(buffSize) * 1.05)
	}
}

func (lm *LevelManager) StoreLastFlushedVersion(version int64, reprocess bool, replSeq int) error {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	log.Debugf("in levelmanager StoreLastFlushedVersion. version %d reprocess %t replseq %d", version, reprocess, replSeq)
	if err := lm.checkStateForCommand(reprocess); err != nil {
		return err
	}
	if !lm.checkDuplicate(replSeq, reprocess) {
		return nil
	}
	defer lm.updateReplSeq(replSeq)
	lm.masterRecord.lastFlushedVersion = version
	lm.masterRecord.version++
	lm.hasChanges = true
	return nil
}

func (lm *LevelManager) LoadLastFlushedVersion() (int64, error) {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	if lm.state != stateActive {
		return 0, errors.NewTektiteErrorf(errors.Unavailable, "levelManager not active")
	}
	log.Debugf("levelManager LoadLastFlushedVersion: %d", lm.masterRecord.lastFlushedVersion)
	return lm.masterRecord.lastFlushedVersion, nil
}

func (lm *LevelManager) GetLastProcessedReplSeq() int {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return lm.masterRecord.lastProcessedReplSeq
}

func (lm *LevelManager) DumpLevelInfo() {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	lm.dumpLevelInfo()
}

func (lm *LevelManager) dumpLevelInfo() {
	builder := strings.Builder{}
	for level := range lm.masterRecord.levelSegmentEntries {
		tableCount := lm.masterRecord.levelTableCounts[level]
		builder.WriteString(fmt.Sprintf("level:%d table_count:%d, ", level, tableCount))
	}
	log.Debug(builder.String())
}

func (lm *LevelManager) Dump() {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	lm.dump()
}

func (lm *LevelManager) dump() {
	log.Debugf("Dumping LevelManager ====================")
	for level, entries := range lm.masterRecord.levelSegmentEntries {
		log.Debugf("Dumping level %d. There are %d segment entries", level, len(entries.segmentEntries))
		for _, segEntry := range entries.segmentEntries {
			log.Debugf("segment entry %s range start %s range end %s", string(segEntry.segmentID),
				string(segEntry.rangeStart), string(segEntry.rangeEnd))
			seg, err := lm.getSegment(segEntry.segmentID)
			if err != nil {
				panic(err)
			}
			log.Debugf("segment %v has %d table entries", segEntry.segmentID, len(seg.tableEntries))
			for _, te := range seg.tableEntries {
				log.Debugf("table entry sstableid %v (%s) range start %s range end %s deleteRatio %.2f hasDeletes %t", te.SSTableID, string(te.SSTableID),
					string(te.RangeStart), string(te.RangeEnd), te.DeleteRatio, te.DeleteRatio > 0)
			}
		}
	}
	for prefix := range lm.masterRecord.slabRetentions {
		log.Debugf("prefix %v", prefix)
	}
}

func containsTable(seg *segment, tabID sst.SSTableID) bool {
	for _, te := range seg.tableEntries {
		if bytes.Equal(te.SSTableID, tabID) {
			return true
		}
	}
	return false
}

func (lm *LevelManager) GetObjectStore() objstore.Client {
	return lm.objStore
}

func (lm *LevelManager) GetLevelTableCounts() map[int]int {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	counts := map[int]int{}
	for level, count := range lm.masterRecord.levelTableCounts {
		counts[level] = count
	}
	return counts
}

func (lm *LevelManager) GetCompactionStats() CompactionStats {
	lm.lock.RLock()
	defer lm.lock.RUnlock()
	return lm.stats
}

func (lm *LevelManager) GetStats() Stats {
	lm.lock.RLock()
	defer lm.lock.RUnlock()
	statsCopy := lm.masterRecord.stats.copy()
	return *statsCopy
}

func (lm *LevelManager) getDeadVersions() []VersionRange {
	lm.lock.Lock()
	defer lm.lock.Unlock()
	return lm.masterRecord.deadVersionRanges
}
