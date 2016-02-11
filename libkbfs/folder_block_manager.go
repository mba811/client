package libkbfs

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/keybase/client/go/logger"
	"golang.org/x/net/context"
)

type fbmHelper interface {
	getMDForFBM(ctx context.Context) (*RootMetadata, error)
	reembedForFBM(ctx context.Context, rmds []*RootMetadata) error
}

// folderBlockManager is a helper class for managing the blocks in a
// particular TLF.  It archives historical blocks and reclaims quota
// usage, all in the background.
type folderBlockManager struct {
	config       Config
	log          logger.Logger
	shutdownChan chan struct{}
	id           TlfID

	// A queue of MD updates for this folder that need to have their
	// unref's blocks archived
	archiveChan chan *RootMetadata

	archivePauseChan chan (<-chan struct{})

	// archiveGroup tracks the outstanding archives.
	archiveGroup repeatedWaitGroup

	archiveCancelLock sync.Mutex
	archiveCancel     context.CancelFunc

	// blocksToDeleteAfterError is a list of blocks, for a given
	// metadata revision, that may have been Put as part of a failed
	// MD write.  These blocks should be deleted as soon as we know
	// for sure that the MD write isn't visible to others.
	// The lock should only be held immediately around accessing the
	// list.  TODO: Persist these to disk?
	blocksToDeleteLock       sync.Mutex
	blocksToDeleteAfterError map[*RootMetadata][]BlockPointer

	// forceReclamation forces the manager to start a reclamation
	// process.
	forceReclamationChan chan struct{}

	// reclamationGroup tracks the outstanding quota reclamations.
	reclamationGroup repeatedWaitGroup

	reclamationCancelLock sync.Mutex
	reclamationCancel     context.CancelFunc

	helper fbmHelper
}

func newFolderBlockManager(config Config, fb FolderBranch,
	helper fbmHelper) *folderBlockManager {
	tlfStringFull := fb.Tlf.String()
	log := config.MakeLogger(fmt.Sprintf("FBM %s", tlfStringFull[:8]))
	fbm := &folderBlockManager{
		config:                   config,
		log:                      log,
		shutdownChan:             make(chan struct{}),
		id:                       fb.Tlf,
		archiveChan:              make(chan *RootMetadata, 25),
		archivePauseChan:         make(chan (<-chan struct{})),
		blocksToDeleteAfterError: make(map[*RootMetadata][]BlockPointer),
		forceReclamationChan:     make(chan struct{}, 1),
		helper:                   helper,
	}
	// Pass in the BlockOps here so that the archive goroutine
	// doesn't do possibly-racy-in-tests access to
	// fbm.config.BlockOps().
	go fbm.archiveBlocksInBackground()
	if fb.Branch == MasterBranch {
		go fbm.reclaimQuotaInBackground()
	}
	return fbm
}

func (fbm *folderBlockManager) setArchiveCancel(cancel context.CancelFunc) {
	fbm.archiveCancelLock.Lock()
	defer fbm.archiveCancelLock.Unlock()
	fbm.archiveCancel = cancel
}

func (fbm *folderBlockManager) cancelArchive() {
	archiveCancel := func() context.CancelFunc {
		fbm.archiveCancelLock.Lock()
		defer fbm.archiveCancelLock.Unlock()
		archiveCancel := fbm.archiveCancel
		fbm.archiveCancel = nil
		return archiveCancel
	}()
	if archiveCancel != nil {
		archiveCancel()
	}
}

func (fbm *folderBlockManager) setReclamationCancel(cancel context.CancelFunc) {
	fbm.reclamationCancelLock.Lock()
	defer fbm.reclamationCancelLock.Unlock()
	fbm.reclamationCancel = cancel
}

func (fbm *folderBlockManager) cancelReclamation() {
	reclamationCancel := func() context.CancelFunc {
		fbm.reclamationCancelLock.Lock()
		defer fbm.reclamationCancelLock.Unlock()
		reclamationCancel := fbm.reclamationCancel
		fbm.reclamationCancel = nil
		return reclamationCancel
	}()
	if reclamationCancel != nil {
		reclamationCancel()
	}
}

func (fbm *folderBlockManager) shutdown() {
	close(fbm.shutdownChan)
	fbm.cancelArchive()
	fbm.cancelReclamation()
}

func (fbm *folderBlockManager) cleanUpBlockState(
	md *RootMetadata, bps *blockPutState) {
	fbm.blocksToDeleteLock.Lock()
	defer fbm.blocksToDeleteLock.Unlock()
	// Clean up any blocks that may have been orphaned by this
	// failure.
	for _, bs := range bps.blockStates {
		fbm.blocksToDeleteAfterError[md] =
			append(fbm.blocksToDeleteAfterError[md], bs.blockPtr)
	}
}

func (fbm *folderBlockManager) archiveUnrefBlocks(md *RootMetadata) {
	// Don't archive for unmerged revisions, because conflict
	// resolution might undo some of the unreferences.
	if md.MergedStatus() != Merged {
		return
	}

	fbm.archiveGroup.Add(1)
	fbm.archiveChan <- md
}

// archiveUnrefBlocksNoWait enqueues the MD for archiving without
// blocking.  By the time it returns, the archive group has been
// incremented so future waits will block on this archive.  This
// method is for internal use within folderBlockManager only.
func (fbm *folderBlockManager) archiveUnrefBlocksNoWait(md *RootMetadata) {
	// Don't archive for unmerged revisions, because conflict
	// resolution might undo some of the unreferences.
	if md.MergedStatus() != Merged {
		return
	}

	fbm.archiveGroup.Add(1)

	// Don't block if the channel is full; instead do the send in a
	// background goroutine.  We've already done the Add above, so the
	// wait calls should all work just fine.
	select {
	case fbm.archiveChan <- md:
		return
	default:
		go func() { fbm.archiveChan <- md }()
	}
}

func (fbm *folderBlockManager) waitForArchives(ctx context.Context) error {
	return fbm.archiveGroup.Wait(ctx)
}

func (fbm *folderBlockManager) waitForQuotaReclamations(
	ctx context.Context) error {
	return fbm.reclamationGroup.Wait(ctx)
}

func (fbm *folderBlockManager) forceQuotaReclamation() {
	select {
	case fbm.forceReclamationChan <- struct{}{}:
		fbm.reclamationGroup.Add(1)
	default:
	}
}

func (fbm *folderBlockManager) processBlocksToDelete(ctx context.Context) error {
	// also attempt to delete any error references
	var toDelete map[*RootMetadata][]BlockPointer
	func() {
		fbm.blocksToDeleteLock.Lock()
		defer fbm.blocksToDeleteLock.Unlock()
		toDelete = fbm.blocksToDeleteAfterError
		fbm.blocksToDeleteAfterError =
			make(map[*RootMetadata][]BlockPointer)
	}()

	if len(toDelete) == 0 {
		return nil
	}

	toDeleteAgain := make(map[*RootMetadata][]BlockPointer)
	bops := fbm.config.BlockOps()
	for md, ptrs := range toDelete {
		fbm.log.CDebugf(ctx, "Checking deleted blocks for revision %d",
			md.Revision)
		// Make sure that the MD didn't actually become
		// part of the folder history.  (This could happen
		// if the Sync was canceled while the MD put was
		// outstanding.)
		var rmds []*RootMetadata
		var err error
		if md.MergedStatus() == Merged {
			rmds, err = getMergedMDUpdates(ctx, fbm.config, fbm.id,
				md.Revision)
		} else {
			_, rmds, err = getUnmergedMDUpdates(ctx, fbm.config, fbm.id,
				md.BID, md.Revision)
		}
		if err != nil || len(rmds) == 0 {
			toDeleteAgain[md] = ptrs
			continue
		}
		if rmds[0].data.Dir == md.data.Dir {
			// This md is part of the history of the folder,
			// so we shouldn't delete the blocks.
			fbm.log.CDebugf(ctx, "Not deleting blocks from revision %d",
				md.Revision)
			// But, since this MD put seems to have succeeded, we
			// should archive it.
			fbm.log.CDebugf(ctx, "Archiving successful MD revision %d",
				rmds[0].Revision)
			// Don't block on archiving the MD, because that could
			// lead to deadlock.
			fbm.archiveUnrefBlocksNoWait(rmds[0])
			continue
		}

		// Otherwise something else has been written over
		// this MD, so get rid of the blocks.
		fbm.log.CDebugf(ctx, "Cleaning up blocks for failed revision %d",
			md.Revision)

		for _, ptr := range ptrs {
			err := bops.Delete(ctx, md, ptr.ID, ptr)
			// Ignore permanent errors
			_, isPermErr := err.(BServerError)
			if err != nil {
				fbm.log.CWarningf(ctx, "Couldn't delete ref %v: %v", ptr, err)
				if !isPermErr {
					toDeleteAgain[md] = append(toDeleteAgain[md], ptr)
				}
			}
		}
	}

	if len(toDeleteAgain) > 0 {
		func() {
			fbm.blocksToDeleteLock.Lock()
			defer fbm.blocksToDeleteLock.Unlock()
			for md, ptrs := range toDeleteAgain {
				fbm.blocksToDeleteAfterError[md] =
					append(fbm.blocksToDeleteAfterError[md], ptrs...)
			}
		}()
	}

	return nil
}

// CtxFBMTagKey is the type used for unique context tags within
// folderBlockManager
type CtxFBMTagKey int

const (
	// CtxFBMIDKey is the type of the tag for unique operation IDs
	// within folderBlockManager.
	CtxFBMIDKey CtxFBMTagKey = iota
)

// CtxFBMOpID is the display name for the unique operation
// folderBlockManager ID tag.
const CtxFBMOpID = "FBMID"

func (fbm *folderBlockManager) ctxWithFBMID(
	ctx context.Context) context.Context {
	return ctxWithRandomID(ctx, CtxFBMIDKey, CtxFBMOpID, fbm.log)
}

// Run the passed function with a context that's canceled on shutdown.
func (fbm *folderBlockManager) runUnlessShutdown(
	fn func(ctx context.Context) error) error {
	ctx := fbm.ctxWithFBMID(context.Background())
	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	errChan := make(chan error, 1)
	go func() {
		errChan <- fn(ctx)
	}()

	select {
	case err := <-errChan:
		return err
	case <-fbm.shutdownChan:
		return errors.New("shutdown received")
	}
}

func (fbm *folderBlockManager) archiveBlocksInBackground() {
	for {
		select {
		case md := <-fbm.archiveChan:
			var ptrs []BlockPointer
			for _, op := range md.data.Changes.Ops {
				ptrs = append(ptrs, op.Unrefs()...)
				for _, update := range op.AllUpdates() {
					ptrs = append(ptrs, update.Unref)
				}
			}
			fbm.runUnlessShutdown(func(ctx context.Context) (err error) {
				defer fbm.archiveGroup.Done()
				// This func doesn't take any locks, though it can
				// block md writes due to the buffered channel.  So
				// use the long timeout to make sure things get
				// unblocked eventually, but no need for a short timeout.
				ctx, cancel := context.WithTimeout(ctx, backgroundTaskTimeout)
				fbm.setArchiveCancel(cancel)
				defer fbm.cancelArchive()

				fbm.log.CDebugf(ctx, "Archiving %d block pointers as a result "+
					"of revision %d", len(ptrs), md.Revision)
				bops := fbm.config.BlockOps()
				err = bops.Archive(ctx, md, ptrs)
				if err != nil {
					fbm.log.CWarningf(ctx, "Couldn't archive blocks: %v", err)
					return err
				}

				// Also see if we can delete any blocks.
				if err := fbm.processBlocksToDelete(ctx); err != nil {
					fbm.log.CDebugf(ctx, "Error deleting blocks: %v", err)
					return err
				}

				return nil
			})
		case unpause := <-fbm.archivePauseChan:
			fbm.runUnlessShutdown(func(ctx context.Context) (err error) {
				fbm.log.CInfof(ctx, "Archives paused")
				// wait to be unpaused
				select {
				case <-unpause:
					fbm.log.CInfof(ctx, "Archives unpaused")
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
		case <-fbm.shutdownChan:
			return
		}
	}
}

// getMostRecentOldEnoughAndGCRevisions returns the most recent MD
// that's older than the unref age, as well as the latest revision
// that was scrubbed by the previous gc op.
func (fbm *folderBlockManager) getMostRecentOldEnoughAndGCRevisions(
	ctx context.Context, head *RootMetadata) (
	mostRecentOldEnoughRev MetadataRevision, lastGCRev MetadataRevision,
	err error) {
	// Walk backwards until we find one that is old enough.  Also,
	// look out for the previous gcOp.
	currHead := head.Revision
	mostRecentOldEnoughRev = MetadataRevisionUninitialized
	lastGCRev = MetadataRevisionUninitialized
	for {
		startRev := currHead - maxMDsAtATime + 1 // (MetadataRevision is signed)
		if startRev < MetadataRevisionInitial {
			startRev = MetadataRevisionInitial
		}

		rmds, err := getMDRange(ctx, fbm.config, fbm.id, NullBranchID, startRev,
			currHead, Merged)
		if err != nil {
			return MetadataRevisionUninitialized,
				MetadataRevisionUninitialized, err
		}

		if err := fbm.helper.reembedForFBM(ctx, rmds); err != nil {
			return MetadataRevisionUninitialized,
				MetadataRevisionUninitialized, err
		}

		numNew := len(rmds)
		now := fbm.config.Clock().Now()
		unrefAge := fbm.config.QuotaReclamationMinUnrefAge()
		for i := len(rmds) - 1; i >= 0; i-- {
			rmd := rmds[i]
			if mostRecentOldEnoughRev == MetadataRevisionUninitialized {
				// Trust the client-provided timestamp -- it's
				// possible that a writer with a bad clock could cause
				// another writer to clear out quotas early.  That's
				// ok, there's nothing we can really do about that.
				mtime := time.Unix(0, rmd.data.Dir.Mtime)
				fbm.log.CDebugf(ctx, "Checking mtime %s on revision %d "+
					"against unref age %s", mtime, rmd.Revision, unrefAge)
				if mtime.Add(unrefAge).Before(now) {
					fbm.log.CDebugf(ctx, "Revision %d is older than the unref "+
						"age %s", rmd.Revision, unrefAge)
					mostRecentOldEnoughRev = rmd.Revision
				}
			}

			if lastGCRev == MetadataRevisionUninitialized {
				for j := len(rmd.data.Changes.Ops) - 1; j >= 0; j-- {
					gcOp, ok := rmd.data.Changes.Ops[j].(*gcOp)
					if !ok {
						continue
					}
					fbm.log.CDebugf(ctx, "Found last gc op: %s", gcOp)
					lastGCRev = gcOp.LatestRev
					break
				}
			}

			// Once both return values are set, we are done
			if mostRecentOldEnoughRev != MetadataRevisionUninitialized &&
				lastGCRev != MetadataRevisionUninitialized {
				return mostRecentOldEnoughRev, lastGCRev, nil
			}
		}

		if numNew > 0 {
			currHead = rmds[0].Revision - 1
		}

		if numNew < maxMDsAtATime || currHead < MetadataRevisionInitial {
			break
		}
	}

	return mostRecentOldEnoughRev, lastGCRev, nil
}

// getUnrefBlocks returns a slice containing all the block pointers
// that were unreferenced after the earliestRev, up to and including
// those in latestRev.
func (fbm *folderBlockManager) getUnreferencedBlocks(
	ctx context.Context, latestRev MetadataRevision,
	earliestRev MetadataRevision) (ptrs []BlockPointer, err error) {
	defer func() {
		if err == nil {
			fbm.log.CDebugf(ctx, "Found %d pointers to clean between "+
				"revisions %d and %d", len(ptrs), earliestRev, latestRev)
		}
	}()

	if latestRev <= earliestRev {
		// Nothing to do.
		fbm.log.CDebugf(ctx, "Latest rev %d is included in the previous "+
			"gc op (%d)", latestRev, earliestRev)
		return nil, nil
	}

	// Walk backward, starting from latestRev, until just after
	// earliestRev, gathering block pointers.
	currHead := latestRev
	for {
		startRev := currHead - maxMDsAtATime + 1 // (MetadataRevision is signed)
		if startRev < MetadataRevisionInitial {
			startRev = MetadataRevisionInitial
		}

		rmds, err := getMDRange(ctx, fbm.config, fbm.id, NullBranchID, startRev,
			currHead, Merged)
		if err != nil {
			return nil, err
		}

		if err := fbm.helper.reembedForFBM(ctx, rmds); err != nil {
			return nil, err
		}

		numNew := len(rmds)
		for i := len(rmds) - 1; i >= 0; i-- {
			rmd := rmds[i]
			if rmd.Revision <= earliestRev {
				return ptrs, nil
			}
			for _, op := range rmd.data.Changes.Ops {
				ptrs = append(ptrs, op.Unrefs()...)
				for _, update := range op.AllUpdates() {
					ptrs = append(ptrs, update.Unref)
				}
			}
		}

		if numNew > 0 {
			currHead = rmds[0].Revision - 1
		}

		if numNew < maxMDsAtATime || currHead < MetadataRevisionInitial {
			break
		}
	}

	return ptrs, nil
}

func (fbm *folderBlockManager) doReclamation(timer *time.Timer) (err error) {
	ctx, cancel := context.WithCancel(fbm.ctxWithFBMID(context.Background()))
	fbm.setReclamationCancel(cancel)
	defer fbm.cancelReclamation()
	fbm.log.CDebugf(ctx, "Starting quota reclamation process")
	defer func() {
		fbm.log.CDebugf(ctx, "Ending quota reclamation process: %v", err)
	}()
	defer timer.Reset(fbm.config.QuotaReclamationPeriod())
	defer fbm.reclamationGroup.Done()

	ctx, _ = context.WithTimeout(ctx, backgroundTaskTimeout)

	// First get the current head, and see if we're staged or not.
	head, err := fbm.helper.getMDForFBM(ctx)
	if err != nil {
		return err
	}
	if head.MergedStatus() != Merged {
		return errors.New("Skipping quota reclamation while unstaged")
	}

	// Then grab the lock for this folder, so we're the only one doing
	// garbage collection for a while.
	locked, err := fbm.config.MDServer().TruncateLock(ctx, fbm.id)
	if err != nil {
		return err
	}
	if !locked {
		fbm.log.CDebugf(ctx, "Couldn't get the truncate lock")
		return fmt.Errorf("Couldn't get the truncate lock for folder %d",
			fbm.id)
	}
	defer func() {
		_, unlockErr := fbm.config.MDServer().TruncateUnlock(ctx, fbm.id)
		if unlockErr != nil {
			fbm.log.CDebugf(ctx, "Couldn't release the truncate lock: %v",
				unlockErr)
		}
	}()

	mostRecentOldEnoughRev, lastGCRev, err :=
		fbm.getMostRecentOldEnoughAndGCRevisions(ctx, head)
	if err != nil {
		return err
	}
	if mostRecentOldEnoughRev == MetadataRevisionUninitialized {
		fbm.log.CDebugf(ctx, "No old-enough revisions")
		return nil
	}

	_, err = fbm.getUnreferencedBlocks(ctx, mostRecentOldEnoughRev, lastGCRev)
	if err != nil {
		return err
	}

	return nil
}

func (fbm *folderBlockManager) reclaimQuotaInBackground() {
	timer := time.NewTimer(fbm.config.QuotaReclamationPeriod())
	for {
		// Don't let the timer fire if auto-reclamation is turned off.
		if fbm.config.QuotaReclamationPeriod().Seconds() == 0 {
			timer.Stop()
		}
		select {
		case <-fbm.shutdownChan:
			return
		case <-timer.C:
			fbm.reclamationGroup.Add(1)
		case <-fbm.forceReclamationChan:
		}

		fbm.doReclamation(timer)
	}
}
