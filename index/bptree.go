package index

import (
	"time"

	"go.etcd.io/bbolt"
)

const defaultBatchSize = 100000

// BPTreeOptions options for creating a new bptree.
type BPTreeOptions struct {
	// DirPath path to store index data.
	DirPath string
	// IndexType bptree(bolt).
	IndexType IndexerType
	// ColumnFamilyName db column family name, must be unique.
	ColumnFamilyName string
	// BucketName usually the the same as column family name, must be unique.
	BucketName []byte
	// BatchSize flush batch size.
	BatchSize int
}

// BPTree is a standard b+tree used to store index data.
type BPTree struct {
	opts BPTreeOptions
	db   *bbolt.DB
	// todo
	metedatadb *bbolt.DB
}

// SetType self-explanatory.
func (bo *BPTreeOptions) SetType(typ IndexerType) {
	bo.IndexType = typ
}

// SetColumnFamilyName self-explanatory.
func (bo *BPTreeOptions) SetColumnFamilyName(cfName string) {
	bo.ColumnFamilyName = cfName
}

// SetDirPath self-explanatory.
func (bo *BPTreeOptions) SetDirPath(dirPath string) {
	bo.DirPath = dirPath
}

// GetType self-explanatory.
func (bo *BPTreeOptions) GetType() IndexerType {
	return bo.IndexType
}

// GetColumnFamilyName self-explanatory.
func (bo *BPTreeOptions) GetColumnFamilyName() string {
	return bo.ColumnFamilyName
}

// GetDirPath self-explanatory.
func (bo *BPTreeOptions) GetDirPath() string {
	return bo.DirPath
}

// NewBPTree create a boltdb instance.
// A file can only be opened once. if not, file lock competition will occur.
func NewBPTree(opt BPTreeOptions) (*BPTree, error) {
	if err := checkBPTreeOptions(opt); err != nil {
		return nil, err
	}

	// open metadatadb and db
	path := opt.DirPath + separator + opt.GetColumnFamilyName()
	metaDatadb, err := bbolt.Open(path+metaFileSuffixName, 0600, &bbolt.Options{
		Timeout:         1 * time.Second,
		NoSync:          true,
		InitialMmapSize: 1024,
	})
	if err != nil {
		return nil, err
	}

	db, err := bbolt.Open(path+indexFileSuffixName, 0600, &bbolt.Options{
		Timeout:         1 * time.Second,
		NoSync:          true,
		InitialMmapSize: 1024,
	})
	if err != nil {
		return nil, err
	}

	// open metadatadb and db TX
	metaDatadbTx, err := metaDatadb.Begin(true)
	if err != nil {
		return nil, err
	}

	tx, err := db.Begin(true)
	if err != nil {
		return nil, err
	}

	// cas create bucket
	if _, err := metaDatadbTx.CreateBucketIfNotExists([]byte("meta")); err != nil {
		return nil, err
	}

	if _, err := tx.CreateBucketIfNotExists(opt.BucketName); err != nil {
		return nil, err
	}

	// commit operation
	if err := metaDatadbTx.Commit(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	b := &BPTree{
		metedatadb: metaDatadb,
		db:         db,
		opts:       opt,
	}
	return b, nil
}

// Put method starts a transaction.
// This method writes kv according to the bucket, and creates it if the bucket name does not exist.
func (b *BPTree) Put(key, value []byte) (err error) {
	var tx *bbolt.Tx
	if tx, err = b.db.Begin(true); err != nil {
		return
	}
	bucket := tx.Bucket(b.opts.BucketName)
	if err = bucket.Put(key, value); err != nil {
		_ = tx.Rollback()
		return
	}
	return tx.Commit()
}

// PutBatch is used for batch writing scenarios.
// The offset marks the transaction write position of the current batch.
// If this function fails during execution, we can write again from the offset position.
// If offset == len(kv) - 1 , all writes are successful.
func (b *BPTree) PutBatch(nodes []*IndexerNode) (offset int, err error) {
	batchLoopNum := len(nodes) / b.opts.BatchSize
	if len(nodes)%b.opts.BatchSize > 0 {
		batchLoopNum++
	}

	batchlimit := b.opts.BatchSize
	for batchIdx := 0; batchIdx < batchLoopNum; batchIdx++ {
		offset = batchIdx * batchlimit
		tx, err := b.db.Begin(true)
		if err != nil {
			return offset, err
		}

		bucket := tx.Bucket(b.opts.BucketName)

	itemLoop:
		for itemIdx := offset; itemIdx < offset+b.opts.BatchSize; itemIdx++ {
			if itemIdx >= len(nodes) {
				break itemLoop
			}
			meta := encodeMeta(nodes[itemIdx].Meta)
			if err := bucket.Put(nodes[itemIdx].Key, meta); err != nil {
				_ = tx.Rollback()
				return offset, err
			}
		}
		if err := tx.Commit(); err != nil {
			return offset, err
		}
	}
	return len(nodes) - 1, nil
}

// DeleteBatch delete data in batch.
func (b *BPTree) DeleteBatch(keys [][]byte) error {
	batchLoopNum := len(keys) / b.opts.BatchSize
	if len(keys)%b.opts.BatchSize > 0 {
		batchLoopNum++
	}

	batchlimit := b.opts.BatchSize
	for batchIdx := 0; batchIdx < batchLoopNum; batchIdx++ {
		offset := batchIdx * batchlimit
		tx, err := b.db.Begin(true)
		if err != nil {
			return err
		}
		bucket := tx.Bucket(b.opts.BucketName)

	itemLoop:
		for itemIdx := offset; itemIdx < offset+b.opts.BatchSize; itemIdx++ {
			if itemIdx >= len(keys) {
				break itemLoop
			}
			if err := bucket.Delete(keys[itemIdx]); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Delete a specified key from indexer.
func (b *BPTree) Delete(key []byte) error {
	return b.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(b.opts.BucketName).Delete(key)
	})
}

// Get reads the value from the bucket with key.
func (b *BPTree) Get(key []byte) (*IndexerMeta, error) {
	tx, err := b.db.Begin(false)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	buf := tx.Bucket(b.opts.BucketName).Get(key)
	return decodeMeta(buf), nil
}

// Sync executes fdatasync() against the database file handle.
func (b *BPTree) Sync() error {
	if err := b.db.Sync(); err != nil {
		return err
	}
	if err := b.metedatadb.Sync(); err != nil {
		return err
	}
	return nil
}

// Close close bolt db.
func (b *BPTree) Close() error {
	if err := b.db.Close(); err != nil {
		return err
	}
	if err := b.metedatadb.Close(); err != nil {
		return err
	}
	return nil
}

func checkBPTreeOptions(opt BPTreeOptions) error {
	if opt.ColumnFamilyName == "" {
		return ErrColumnFamilyNameNil
	}

	if opt.DirPath == "" {
		return ErrDirPathNil
	}

	if opt.BucketName == nil || len(opt.BucketName) == 0 {
		return ErrBucketNameNil
	}

	if opt.BatchSize < defaultBatchSize {
		opt.BatchSize = defaultBatchSize
	}
	return nil
}

type BPTreeIter struct {
	bpTree *BPTree
	bucket *bbolt.Bucket
	tx     *bbolt.Tx
}

func (b *BPTree) Iter() (IndexerIter, error) {
	tx, err := b.db.Begin(false)
	if err != nil {
		return nil, err
	}

	bucket := tx.Bucket(b.opts.BucketName)
	if bucket == nil {
		return nil, ErrBucketNotInit
	}

	return &BPTreeIter{
		bpTree: b,
		bucket: bucket,
		tx:     tx,
	}, nil
}

func (b *BPTreeIter) First() (key, value []byte) {
	return b.bucket.Cursor().First()
}

func (b *BPTreeIter) Last() (key, value []byte) {
	return b.bucket.Cursor().Last()
}

func (b *BPTreeIter) Seek(seek []byte) (key, value []byte) {
	return b.bucket.Cursor().Seek(seek)
}

func (b *BPTreeIter) Next() (key, value []byte) {
	return b.bucket.Cursor().Next()
}

func (b *BPTreeIter) Prev() (key, value []byte) {
	return b.bucket.Cursor().Prev()
}

func (b *BPTreeIter) Close() (err error) {
	return b.tx.Rollback()
}
