package cli

import (
	"context"
	"fmt"
	"runtime"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"
)

type commandSnapshotVerify struct {
	verifyCommandErrorThreshold int
	verifyCommandDirObjectIDs   []string
	verifyCommandFileObjectIDs  []string
	verifyCommandSnapshotIDs    []string
	verifyCommandAllSources     bool
	verifyCommandSources        []string
	verifyCommandParallel       int
	verifyCommandFilesPercent   float64

	fileQueueLength int
	fileParallelism int

	jo  jsonOutput
	out textOutput
}

func (c *commandSnapshotVerify) setup(svc appServices, parent commandParent) {
	c.fileParallelism = runtime.NumCPU()

	cmd := parent.Command("verify", "Verify the contents of stored snapshot")
	cmd.Arg("snapshot-ids", "snapshot IDs to verify").StringsVar(&c.verifyCommandSnapshotIDs)
	cmd.Flag("max-errors", "Maximum number of errors before stopping").Default("0").IntVar(&c.verifyCommandErrorThreshold)
	cmd.Flag("directory-id", "Directory object IDs to verify").StringsVar(&c.verifyCommandDirObjectIDs)
	cmd.Flag("file-id", "File object IDs to verify").StringsVar(&c.verifyCommandFileObjectIDs)
	cmd.Flag("all-sources", "Verify all snapshots (DEPRECATED)").Hidden().BoolVar(&c.verifyCommandAllSources)
	cmd.Flag("sources", "Verify the provided sources").StringsVar(&c.verifyCommandSources)
	cmd.Flag("parallel", "Parallelization").Default("8").IntVar(&c.verifyCommandParallel)
	cmd.Flag("file-queue-length", "Queue length for file verification").Default("20000").IntVar(&c.fileQueueLength)
	cmd.Flag("file-parallelism", "Parallelism for file verification").IntVar(&c.fileParallelism)
	cmd.Flag("verify-files-percent", "Randomly verify a percentage of files by downloading them [0.0 .. 100.0]").Default("0").Float64Var(&c.verifyCommandFilesPercent)

	c.jo.setup(svc, cmd)
	c.out.setup(svc)

	cmd.Action(svc.repositoryReaderAction(c.run))
}

func (c *commandSnapshotVerify) run(ctx context.Context, rep repo.Repository) error {
	if c.verifyCommandAllSources {
		log(ctx).Error("DEPRECATED: --all-sources flag has no effect and is the default when no sources are provided.")
	}

	if dr, ok := rep.(repo.DirectRepositoryWriter); ok {
		dr.DisableIndexRefresh()
	}

	opts := snapshotfs.VerifierOptions{
		VerifyFilesPercent: c.verifyCommandFilesPercent,
		FileQueueLength:    c.fileQueueLength,
		Parallelism:        c.fileParallelism,
		MaxErrors:          c.verifyCommandErrorThreshold,
		JSONStats:          c.jo.jsonOutput,
	}

	if dr, ok := rep.(repo.DirectRepository); ok {
		blobMap, err := blob.ReadBlobMap(ctx, dr.BlobReader())
		if err != nil {
			return errors.Wrap(err, "unable to read blob map")
		}

		opts.BlobMap = blobMap
	}

	v := snapshotfs.NewVerifier(ctx, rep, opts)

	defer func() {
		// Suppress final stats output if --json flag provided.
		if !c.jo.jsonOutput {
			v.ShowFinalStats(ctx)
		}
	}()

	result, err := v.InParallel(ctx, c.makeVerifyWalkerFunc(ctx, rep, v))

	if c.jo.jsonOutput {
		c.out.printStdout("%s\n", c.jo.jsonIndentedBytes(result, "  "))
	}

	//nolint:wrapcheck
	return err
}

func (c *commandSnapshotVerify) makeVerifyWalkerFunc(ctx context.Context, rep repo.Repository, v *snapshotfs.Verifier) func(tw *snapshotfs.TreeWalker) error {
	return func(tw *snapshotfs.TreeWalker) error {
		manifests, err := c.loadSourceManifests(ctx, rep)
		if err != nil {
			return err
		}

		snapIDManifests, err := c.loadSnapIDManifests(ctx, rep)
		if err != nil {
			return err
		}

		manifests = append(manifests, snapIDManifests...)

		type twEntry struct {
			root     fs.Entry
			rootPath string
		}

		var treeWalkerEntries []twEntry

		for _, man := range manifests {
			rootPath := fmt.Sprintf("%v@%v", man.Source, formatTimestamp(man.StartTime.ToTime()))

			if man.RootEntry == nil {
				continue
			}

			root, err := snapshotfs.SnapshotRoot(rep, man)
			if err != nil {
				return errors.Wrapf(err, "unable to get snapshot root: %q", rootPath)
			}

			treeWalkerEntries = append(treeWalkerEntries, twEntry{
				root:     root,
				rootPath: rootPath,
			})

			if err := addExpectedWorkFromDirSummaryToVerifier(ctx, v, root); err != nil {
				return errors.Wrapf(err, "unable to set stat totals from summary")
			}
		}

		for _, twEntry := range treeWalkerEntries {
			// ignore error now, return aggregate error at a higher level.
			//nolint:errcheck
			tw.Process(ctx, twEntry.root, twEntry.rootPath)
		}

		for _, oidStr := range c.verifyCommandDirObjectIDs {
			oid, err := snapshotfs.ParseObjectIDWithPath(ctx, rep, oidStr)
			if err != nil {
				return errors.Wrapf(err, "unable to parse: %q", oidStr)
			}

			// ignore error now, return aggregate error at a higher level.
			//nolint:errcheck
			tw.Process(ctx, snapshotfs.DirectoryEntry(rep, oid, nil), oidStr)
		}

		for _, oidStr := range c.verifyCommandFileObjectIDs {
			oid, err := snapshotfs.ParseObjectIDWithPath(ctx, rep, oidStr)
			if err != nil {
				return errors.Wrapf(err, "unable to parse %q", oidStr)
			}

			// ignore error now, return aggregate error at a higher level.
			//nolint:errcheck
			tw.Process(ctx, snapshotfs.AutoDetectEntryFromObjectID(ctx, rep, oid, oidStr), oidStr)
		}

		return nil
	}
}

// addExpectedWorkFromDirSummaryToVerifier initializes the snapshot verifier with an
// expected amount of work that will take place during the tree walk for this Entry.
// If the entry is not a DirectoryWithSummary, or the Summary returns nil, no stats
// will be added to the totals.
func addExpectedWorkFromDirSummaryToVerifier(ctx context.Context, v *snapshotfs.Verifier, ent fs.Entry) error {
	dws, ok := ent.(fs.DirectoryWithSummary)
	if !ok {
		// Entry is not a directory with summary, no stats to add.
		return nil
	}

	s, err := dws.Summary(ctx)
	if err != nil {
		return errors.Wrap(err, "unable to get directory summary")
	}

	if s == nil {
		// Summary returned nil, no stats to add.
		return nil
	}

	// Add the maximum expected work that could be done by this walk.
	// This can be used to gauge progress.
	v.AddToExpectedTotals(
		s.TotalFileCount+s.TotalDirCount+s.TotalSymlinkCount,
		s.TotalFileCount,
		s.TotalFileSize,
	)

	return nil
}

func (c *commandSnapshotVerify) loadSourceManifests(ctx context.Context, rep repo.Repository) ([]*snapshot.Manifest, error) {
	var manifestIDs []manifest.ID

	if c.noVerifyTargetArgsProvided() {
		// User didn't specify any particular snapshot or snapshots to verify.
		// Read out all manifests and verify everything.
		man, err := snapshot.ListSnapshotManifests(ctx, rep, nil, nil)
		if err != nil {
			return nil, errors.Wrap(err, "unable to list snapshot manifests")
		}

		manifestIDs = append(manifestIDs, man...)
	} else {
		for _, srcStr := range c.verifyCommandSources {
			src, err := snapshot.ParseSourceInfo(srcStr, rep.ClientOptions().Hostname, rep.ClientOptions().Username)
			if err != nil {
				return nil, errors.Wrapf(err, "error parsing %q", srcStr)
			}

			man, err := snapshot.ListSnapshotManifests(ctx, rep, &src, nil)
			if err != nil {
				return nil, errors.Wrapf(err, "unable to list snapshot manifests for %v", src)
			}

			manifestIDs = append(manifestIDs, man...)
		}
	}

	//nolint:wrapcheck
	return snapshot.LoadSnapshots(ctx, rep, manifestIDs)
}

// noVerifyTargetArgsProvided will return true if the user didn't specify any
// particular snapshots to be verified, by any of the available means.
// This can be used to determine whether all snapshots should be verified.
func (c *commandSnapshotVerify) noVerifyTargetArgsProvided() bool {
	return len(c.verifyCommandSources) == 0 &&
		len(c.verifyCommandDirObjectIDs) == 0 &&
		len(c.verifyCommandFileObjectIDs) == 0 &&
		len(c.verifyCommandSnapshotIDs) == 0
}

// loadSnapIDManifests will return the list of manifests requested by the
// snapshot verify Arg values, to be interpreted as manifest IDs.
func (c *commandSnapshotVerify) loadSnapIDManifests(ctx context.Context, rep repo.Repository) ([]*snapshot.Manifest, error) {
	manifestIDs := toManifestIDs(c.verifyCommandSnapshotIDs)

	manifests, err := snapshot.LoadSnapshots(ctx, rep, manifestIDs)
	if err != nil {
		return nil, errors.Wrap(err, "unable to load snapshot manifests")
	}

	if len(manifests) != len(manifestIDs) {
		return nil, errors.Errorf("found %d of the %d requested snapshot IDs to verify", len(manifests), len(manifestIDs))
	}

	return manifests, nil
}
