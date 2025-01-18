package archive

import (
	"context"
	"errors"
	"fmt"

	"github.com/adammck/archive/pkg/blobstore"
	"github.com/adammck/archive/pkg/compactor"
	"github.com/adammck/archive/pkg/memtable"
	"github.com/adammck/archive/pkg/metadata"
	"github.com/adammck/archive/pkg/sstable"
	"github.com/adammck/archive/pkg/types"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"
)

type Archive struct {
	mt    *memtable.Memtable
	bs    *blobstore.Blobstore
	md    *metadata.Store
	clock clockwork.Clock
	comp  *compactor.Compactor
}

func New(mongoURL, bucket string, clock clockwork.Clock) *Archive {
	bs := blobstore.New(bucket, clock)
	md := metadata.New(mongoURL)

	return &Archive{
		mt:    memtable.New(mongoURL, clock),
		bs:    bs,
		md:    md,
		clock: clock,
		comp:  compactor.New(bs, md, clock),
	}
}

func (a *Archive) Ping(ctx context.Context) error {
	err := a.mt.Ping(ctx)
	if err != nil {
		return fmt.Errorf("memtable.Ping: %w", err)
	}

	err = a.bs.Ping(ctx)
	if err != nil {
		return fmt.Errorf("blobstore.Ping: %w", err)
	}

	return nil
}

func (a *Archive) Init(ctx context.Context) error {
	err := a.mt.Init(ctx)
	if err != nil {
		return fmt.Errorf("memtable.Init: %s", err)
	}

	err = a.md.Init(ctx)
	if err != nil {
		return fmt.Errorf("metadata.Init: %s", err)
	}

	return nil
}

func (a *Archive) Put(ctx context.Context, key string, value []byte) (string, error) {
	return a.mt.Put(ctx, key, value)
}

type GetStats struct {
	Source         string
	BlobsFetched   int
	RecordsScanned int
}

// TODO: return the Record, or maybe the timestamp too, not just the value.
func (a *Archive) Get(ctx context.Context, key string) (value []byte, stats *GetStats, err error) {
	stats = &GetStats{}

	rec, src, err := a.mt.Get(ctx, key)
	if err != nil && !errors.Is(err, &memtable.NotFound{}) {
		return nil, stats, fmt.Errorf("memtable.Get: %w", err)
	}
	if rec != nil {
		// TODO: Update Memtable.Get to return stats too.
		stats.Source = src
		return rec.Document, stats, nil
	}

	metas, err := a.md.GetContaining(ctx, key)
	if err != nil {
		return nil, stats, fmt.Errorf("metadata.GetContaining: %w", err)
	}

	// note: this assumes that metas is already sorted.
	for _, meta := range metas {
		rec, bstats, err := a.bs.Find(ctx, meta.Filename(), key)
		if err != nil {
			return nil, stats, fmt.Errorf("blobstore.Get: %w", err)
		}

		// accumulate stats as we go
		stats.BlobsFetched++
		stats.RecordsScanned += bstats.RecordsScanned

		if rec != nil {
			// return as soon as we find the first record, but that's wrong!
			// before returning, we need to look at the record timestamp, and
			// check whether any of the remaining metas have a minTime newer
			// than that. this is only possible after a weird compaction.
			// TODO: fix this!
			stats.Source = bstats.Source
			return rec.Document, stats, nil
		}
	}

	// key not found
	return nil, stats, nil
}

type FlushStats struct {

	// The URL of the memtable which was flushed.
	FlushedMemtable string

	// The URL of the memtable that is now active, after the flush.
	ActiveMemtable string

	// The URL of the flushed sstable.
	BlobURL string

	// Metadata about the flushed sstable.
	Meta *sstable.Meta
}

func (a *Archive) Flush(ctx context.Context) (*FlushStats, error) {
	stats := &FlushStats{}

	// TODO: check whether old sstable is still flushing
	handle, mt, err := a.mt.Swap(ctx)
	if err != nil {
		return stats, fmt.Errorf("switchMemtable: %s", err)
	}

	// TODO: fix this by moving URL from Memtable to Handle.
	//stats.FlushedMemtable = handle.URL()
	stats.ActiveMemtable = mt

	ch := make(chan *types.Record)
	g, ctx2 := errgroup.WithContext(ctx)

	g.Go(func() error {
		var err error
		err = handle.Flush(ctx2, ch)
		if err != nil {
			return fmt.Errorf("memtable.Flush: %w", err)
		}
		return nil
	})

	var dest string
	var meta *sstable.Meta

	g.Go(func() error {
		var err error
		dest, _, meta, err = a.bs.Flush(ctx2, ch)
		if err != nil {
			return fmt.Errorf("blobstore.Flush: %w", err)
		}
		return nil
	})

	err = g.Wait()
	if err != nil {
		return stats, err
	}

	err = a.md.Insert(ctx, meta)
	if err != nil {
		return stats, fmt.Errorf("metadata.Insert: %w", err)
	}

	stats.BlobURL = dest
	stats.Meta = meta

	err = handle.Truncate(ctx)
	if err != nil {
		return stats, fmt.Errorf("handle.Truncate: %w", err)
	}

	return stats, nil
}

type CompactionStats = compactor.CompactionStats
type CompactionOptions = compactor.CompactionOptions

func (a *Archive) Compact(ctx context.Context, opts CompactionOptions) ([]*CompactionStats, error) {
	return a.comp.Run(ctx, opts)
}
