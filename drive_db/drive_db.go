// drive_sync syncs Google Drive metadata to a local LevelDB database and provides methods to query it.

package drive_db

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"sync"
	"time"

	gdrive "code.google.com/p/google-api-go-client/drive/v2"
	"github.com/asjoyner/fuse_gdrive/lru"
	"github.com/golang/groupcache/singleflight"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
)

const downloadUrlLifetime = time.Duration(time.Hour * 12)

var debugDriveDB = flag.Bool("drivedb.debug", false, "print debug statements from the drive_db package")

type debugging bool

var debug debugging

func (d debugging) Printf(format string, args ...interface{}) {
	if d {
		log.Printf(format, args...)
	}
}

// encode returns the item encoded into []byte.
func encode(item interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	err := enc.Encode(item)
	return buf.Bytes(), err
}

// decode decodes the data buffer into the item.
func decode(data []byte, item interface{}) error {
	dec := json.NewDecoder(bytes.NewBuffer(data))
	return dec.Decode(item)
}

type File struct {
	*gdrive.File
	Inode                 uint64
	Children              []uint64 // inodes of children
	cachedDownloadUrl     string
	cachedDownloadUrlTime time.Time
}

type CheckPoint struct {
	LastChangeID int64
	LastInode    uint64
}

type DriveDB struct {
	sync.Mutex
	service      *gdrive.Service
	db           *leveldb.DB
	syncmu       sync.Mutex
	synced       *sync.Cond
	iters        sync.WaitGroup
	cpt          CheckPoint
	changes      chan *gdrive.ChangeList
	lruCache     *lru.Cache // inode to *File
	pollInterval time.Duration
	sf           singleflight.Group
}

// NewDriveDB creates a new DriveDB and starts syncing.
func NewDriveDB(svc *gdrive.Service, filepath string, pollInterval time.Duration) (*DriveDB, error) {
	if *debugDriveDB {
		debug = true
	}

	o := &opt.Options{
		Filter: filter.NewBloomFilter(10),
		Strict: opt.StrictAll,
	}
	db, err := leveldb.OpenFile(filepath, o)
	if err != nil {
		if _, ok := err.(*errors.ErrCorrupted); ok {
			log.Printf("recovering leveldb: %v", err)
			db, err = leveldb.RecoverFile(filepath, o)
			if err != nil {
				log.Printf("failed to recover leveldb: %v", err)
				return nil, err
			}
		} else {
			log.Printf("failed to open leveldb: %v", err)
			return nil, err
		}
	}

	d := &DriveDB{
		service:      svc,
		db:           db,
		lruCache:     lru.New(int(1000)), // make the value tunable
		changes:      make(chan *gdrive.ChangeList, 200),
		pollInterval: pollInterval,
	}

	// Get saved checkpoint.
	err = d.get(internalKey("checkpoint"), &d.cpt)
	if err != nil {
		log.Printf("error reading checkpoint: %v", err)
		d.cpt.LastInode = 1000 // start high, to allow "special" inodes
	}
	err = d.writeCheckpoint(nil)
	if err != nil {
		return nil, fmt.Errorf("could not write checkpoint: %v", err)
	}
	debug.Printf("Recovered from checkpoint: %+v", d.cpt)

	d.synced = sync.NewCond(&d.syncmu)

	go d.sync()
	go d.pollForChanges()
	// TODO: flag wrap this, it has serious privacy and access concerns
	registerDebugHandles(*d)  // in http_handlers.go
	return d, nil
}

// LevelDB key helpers. Key prefixes are 3 chars and ":".
func internalKey(key string) []byte {
	return []byte("int:" + key)
}

func fileIdToInodeKey(key string) []byte {
	return []byte("f2i:" + key)
}

func inodeToFileIdKey(key uint64) []byte {
	return []byte("i2f:" + fmt.Sprintf("%d", key))
}

func fileKey(key string) []byte {
	return []byte("fid:" + key)
}

func childKey(key string) []byte {
	return []byte("kid:" + key)
}

func rootKey(key string) []byte {
	return []byte("rtf:" + key)
}

func deKey(key string) string {
	return key[4:]
}

// get retrives a single key from the database.
func (d *DriveDB) get(key []byte, item interface{}) error {
	data, err := d.db.Get(key, nil)
	if err != nil {
		return err
	}
	return decode(data, item)
}

// writeCheckpoint writes the checkpoint to the db, optionally using a batch.
func (d *DriveDB) writeCheckpoint(batch *leveldb.Batch) error {
	d.Lock()
	cpt := d.cpt
	d.Unlock()
	bytes, err := encode(cpt)
	if err != nil {
		log.Printf("error encoding checkpoint: %v", err)
		return err
	}
	if batch != nil {
		batch.Put(internalKey("checkpoint"), bytes)
		return nil
	}
	return d.db.Put(internalKey("checkpoint"), bytes, nil)
}

// lastChangeId() returns the last changeID recorded in the checkpoint.
func (d *DriveDB) lastChangeId() int64 {
	d.Lock()
	defer d.Unlock()
	return d.cpt.LastChangeID
}

// setLastChangeId sets the lastChangeId in the checkpoint.
// It does not commit to leveldb; use writeCheckpoint to do that.
func (d *DriveDB) setLastChangeId(id int64) {
	d.Lock()
	defer d.Unlock()
	d.cpt.LastChangeID = id
}

// nextInode allocates a new inode number and updates the checkpoint, including writing to leveldb.
func (d *DriveDB) nextInode(batch *leveldb.Batch) (uint64, error) {
	var inode uint64
	d.Lock()
	d.cpt.LastInode++
	inode = d.cpt.LastInode
	d.Unlock()
	return inode, d.writeCheckpoint(batch)
}

// InodeForFileId returns a File's inode number, given its ID.
// Allocates a new inode number if needed, and commits immediately
// to leveldb.
func (d *DriveDB) InodeForFileId(fileId string) (uint64, error) {
	key := "inf:" + fileId
	v, err := d.sf.Do(key, func() (interface{}, error) {
		return d.inodeForFileIdImpl(fileId)
	})
	return v.(uint64), err
}

func (d *DriveDB) inodeForFileIdImpl(fileId string) (uint64, error) {
	var inode uint64
	err := d.get(fileIdToInodeKey(fileId), &inode)
	if err == nil {
		// return what we have.
		return inode, nil
	}

	batch := new(leveldb.Batch)

	// allocate an inode number
	inode, err = d.nextInode(batch)
	if err != nil {
		return 0, err
	}

	encodedInode, err := encode(inode)
	if err != nil {
		return 0, err
	}

	encodedFileId, err := encode(fileId)
	if err != nil {
		return 0, err
	}

	// Create forward and reverse mappings.
	batch.Put(fileIdToInodeKey(fileId), encodedInode)
	batch.Put(inodeToFileIdKey(inode), encodedFileId)
	err = d.db.Write(batch, nil)
	if err != nil {
		return 0, err
	}
	return inode, nil
}

// AllFileIds returns the IDs of all Google Drive file objects currently stored.
func (d *DriveDB) AllFileIds() ([]string, error) {
	var ids []string
	// We can't Close() until all iterators are released.
	// TODO: this can still be racy with Close(), fix that.
	d.iters.Add(1)
	iter := d.db.NewIterator(util.BytesPrefix(fileKey("")), nil)
	for iter.Next() {
		ids = append(ids, deKey(string(iter.Key())))
	}
	iter.Release()
	d.iters.Done()
	return ids, iter.Error()
}

// RootFileIds returns the IDs of all Google Drive file objects at the root.
func (d *DriveDB) RootFileIds() ([]string, error) {
	var ids []string
	d.iters.Add(1)
	iter := d.db.NewIterator(util.BytesPrefix(rootKey("")), nil)
	for iter.Next() {
		ids = append(ids, deKey(string(iter.Key())))
	}
	iter.Release()
	d.iters.Done()
	return ids, iter.Error()
}

// RootInodes returns the inodes of all Google Drive file objects that are
// children of the root.
func (d *DriveDB) RootInodes() ([]uint64, error) {
	f, ok := d.lruCache.Get("rootInodes")
	if ok {
		return f.([]uint64), nil
	}

	var ids []uint64
	fids, err := d.RootFileIds()
	if err != nil {
		return ids, err
	}
	for _, fid := range fids {
		inode, err := d.InodeForFileId(fid)
		if err == nil {
			ids = append(ids, inode)
		}
	}

	d.lruCache.Add("rootInodes", ids)
	return ids, nil
}

// ChildFileIds returns the IDs of all Files that have parent refs to the given file.
func (d *DriveDB) ChildFileIds(fileId string) ([]string, error) {
	var ids []string
	d.iters.Add(1)
	batch := new(leveldb.Batch)
	iter := d.db.NewIterator(util.BytesPrefix(childKey(fileId)), nil)
	for iter.Next() {
		pidcid := deKey(string(iter.Key()))
		cid := pidcid[len(fileId)+1:]
		found, err := d.db.Has(fileKey(cid), nil)
		if err == nil && found {
			ids = append(ids, cid)
		} else {
			batch.Delete(iter.Key())
		}
	}
	iter.Release()
	d.iters.Done()
	if batch.Len() > 0 {
		err := d.db.Write(batch, nil)
		if err != nil {
			log.Printf("error writing to db: %v", err)
		}
	}
	return ids, iter.Error()
}

// FileById returns a File, given its ID.
func (d *DriveDB) FileById(fileId string) (*gdrive.File, error) {
	var res gdrive.File
	err := d.get(fileKey(fileId), &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// FileIdForInode returns the FileId associated with a given inode.
func (d *DriveDB) FileIdForInode(inode uint64) (string, error) {
	var fileId string
	err := d.get(inodeToFileIdKey(inode), &fileId)
	if err != nil {
		log.Printf("FileIdForInode: %v: %v", inode, err)
		return "", err
	}
	return fileId, nil
}

// FileByInode
func (d *DriveDB) FileByInode(inode uint64) (*File, error) {
	f, ok := d.lruCache.Get(inode)
	if ok {
		//fmt.Println("Returning cached inode %v", inode)
		return f.(*File), nil
	}

	fileId, err := d.FileIdForInode(inode)
	if err != nil {
		return nil, err
	}

	gdriveFile, err := d.FileById(fileId)
	if err != nil {
		return nil, fmt.Errorf("unknown fileId %v: %v", fileId, err)
	}

	file := File{gdriveFile, 0, nil, "", time.Time{}}
	file.Inode, err = d.InodeForFileId(fileId)
	if err != nil {
		return nil, fmt.Errorf("no inode for %v: %v", fileId, err)
	}

	childFileIds, err := d.ChildFileIds(fileId)
	if err != nil {
		return nil, fmt.Errorf("error getting children of fileId %v: %v", fileId, err)
	}
	file.Children = make([]uint64, len(childFileIds))
	for i, fileId := range childFileIds {
		inode, err := d.InodeForFileId(fileId)
		if err != nil {
			return nil, fmt.Errorf("error getting inode of child %v: %v", fileId, err)
		}
		file.Children[i] = inode
	}

	d.lruCache.Add(inode, &file)
	//fmt.Println("Returning fresh inode %v", inode)
	return &file, nil
}

// Refresh the file object of the given fileId
func (d *DriveDB) Refresh(fileId string) (*File, error) {
	f, err := d.service.Files.Get(fileId).Do()
	if err != nil {
		return &File{}, err
	}
	return d.UpdateFile(nil, f)
}

// The DownloadUrl has a finite lifetime, this ensures we have a fresh cached copy
// hint: "403 Forbidden" is returned when it has expired
func (d *DriveDB) FreshDownloadUrl(f *File) string {
	if f.DownloadUrl == "" {
		return ""
	}
	if time.Since(f.cachedDownloadUrlTime) < downloadUrlLifetime {
		return f.cachedDownloadUrl
	}
	log.Printf("Refreshing DownloadUrl for %v", f.Title)
	fresh, err := d.service.Files.Get(f.Id).Do()
	if err != nil {
		log.Printf("Failed to refresh DownloadUrl: %v", err)
		return f.DownloadUrl
	}
	f.cachedDownloadUrl = fresh.DownloadUrl
	f.cachedDownloadUrlTime = time.Now()
	log.Printf("Cached DownloadUrl for %v for %v", f.Title, downloadUrlLifetime)
	return fresh.DownloadUrl
}

func (d *DriveDB) RemoveFile(f *gdrive.File) error {
	if f == nil {
		return nil
	}
	return d.RemoveFileById(f.Id, nil)
}

func (d *DriveDB) RemoveFileById(fileId string, batch *leveldb.Batch) error {
	if batch == nil {
		batch = new(leveldb.Batch)
	}
	// delete the file itself.
	batch.Delete(fileKey(fileId))
	// delete the inode mapping.
	batch.Delete(fileIdToInodeKey(fileId))
	// delete any "root object" ref
	batch.Delete(rootKey(fileId))
	// also delete all of its child refs
	d.iters.Add(1)
	iter := d.db.NewIterator(util.BytesPrefix(childKey(fileId)), nil)
	for iter.Next() {
		batch.Delete(iter.Key())
	}
	iter.Release()
	d.iters.Done()
	// and delete any parents' refs to it.
	f, err := d.FileById(fileId)
	if err == nil && f != nil {
		for _, pr := range f.Parents {
			batch.Delete(childKey(pr.Id + ":" + fileId))
		}
	}
	err = d.db.Write(batch, nil)
	if err != nil {
		return err
	}
	i, err := d.InodeForFileId(fileId)
	if err == nil {
		// remove from the cache
		d.lruCache.Remove(i)
	}
	return nil
}

// UpdateFile commits a gdrive.File to levelDB, updating all mappings and allocating inodes if needed.
func (d *DriveDB) UpdateFile(batch *leveldb.Batch, f *gdrive.File) (*File, error) {
	if f == nil {
		return &File{}, fmt.Errorf("cannot update nil File")
	}
	fileId := f.Id
	bytes, err := encode(f)
	if err != nil {
		return &File{}, fmt.Errorf("error encoding file %v: %v", fileId, err)
	}

	b := batch
	if b == nil {
		b = new(leveldb.Batch)
	}

	// Wipe the lru cache. We'll re-read elsewhere if needed.
	inode, err := d.InodeForFileId(fileId)
	if err != nil {
		return &File{}, fmt.Errorf("error allocating inode for fileid %v: %v", fileId, err)
	}
	if err == nil && inode > 0 {
		d.lruCache.Remove(inode)
	}

	// write the file itself.
	b.Put(fileKey(fileId), bytes)

	// Maintain child references
	for _, pr := range f.Parents {
		if pr.IsRoot {
			b.Put(rootKey(fileId), []byte{}) // we care only about the key
		} else {
			b.Put(childKey(pr.Id+":"+fileId), []byte{}) // we care only about the key
		}
	}

	// Write now if no batch was supplied.
	if batch == nil {
		err := d.db.Write(batch, nil)
		if err != nil {
			return &File{}, err
		}
	}

	file := File{f, inode, nil, "", time.Time{}}
	return &file, nil
}

func (d *DriveDB) FlushCachedInode(inode uint64) {
	d.lruCache.Remove(inode)
}

// pollForChanges is a background goroutine to poll Drive for changes.
func (d *DriveDB) pollForChanges() {
	poll := make(chan struct{})
	//var trigger struct{}
	pollTime := time.NewTicker(d.pollInterval).C

	d.readChanges()
	for {
		select {
		case <-pollTime:
			d.readChanges()
		case <-poll:
			d.readChanges()
		}
	}
}

// readChanges is called by pollForChanges to grab all new metadata changes from Drive
func (d *DriveDB) readChanges() {
	l := d.service.Changes.List().IncludeDeleted(true).IncludeSubscribed(true).MaxResults(1000)
	lastChangeId := d.lastChangeId()

	if lastChangeId > 0 {
		l.StartChangeId(lastChangeId + 1)
	}

	debug.Printf("Querying Google Drive for changes since %d.", lastChangeId)
	for {
		c, err := l.Do()
		if err != nil {
			log.Printf("sync error: %v", err)
			return
		}
		debug.Printf("Response from Drive contains %d changes of %d", len(c.Items), c.LargestChangeId)

		// Process the changelist.
		d.changes <- c

		if len(c.Items) == 0 {
			return
		}

		// Go to the next page, or next syncid.
		if c.NextPageToken != "" {
			l.PageToken(c.NextPageToken)
		} else {
			return
		}
	}
}

// processChange applies a ChangeList to the database.
func (d *DriveDB) processChange(c *gdrive.ChangeList) error {
	if c == nil {
		return nil
	}

	// If we read zero items, there's no work to do, and we're probably synced.
	if len(c.Items) == 0 {
		if d.lastChangeId() >= c.LargestChangeId {
			d.synced.Broadcast()
		}
		return nil
	}

	log.Printf("processing %v/%v, %v changes", d.lastChangeId(), c.LargestChangeId, len(c.Items))

	batch := new(leveldb.Batch)
	for _, i := range c.Items {
		batch.Reset()
		// Wipe the lru cache for this file. We'll re-read elsewhere if needed.
		inode, err := d.InodeForFileId(i.FileId)
		if err != nil && inode > 0 {
			d.lruCache.Remove(inode)
		}
		// Update leveldb.
		// TODO: don't delete trashed/hidden files? ".trash" folder?
		if i.Deleted || i.File.Labels.Trashed || i.File.Labels.Hidden {
			d.RemoveFileById(i.FileId, batch)
		} else {
			d.UpdateFile(batch, i.File)
		}
		// Update the checkpoint, which now encompasses one additional change.
		d.setLastChangeId(i.Id)
		err = d.writeCheckpoint(batch)
		if err != nil {
			return err
		}
		// Commit
		err = d.db.Write(batch, nil)
		if err != nil {
			return err
		}
	}
	d.lruCache.Remove("rootInodes")
	// Signal we're synced, if we are.
	if d.lastChangeId() >= c.LargestChangeId {
		d.synced.Broadcast()
	}
	return nil
}

// sync is a background goroutine to sync drive data.
func (d *DriveDB) sync() {
	var c *gdrive.ChangeList
	for {
		c = <-d.changes
		err := d.processChange(c)
		if err != nil {
			// TODO: figure out how to recover from the error.
			log.Printf("sync error: %v", err)
		}
	}
}

// WaitUntilSynced blocks until are are synced with Drive.
func (d *DriveDB) WaitUntilSynced() {
	d.synced.L.Lock()
	d.synced.Wait()
	d.synced.L.Unlock()
}

// Close closes DriveDB, waiting until all iterators are closed.
func (d *DriveDB) Close() {
	d.iters.Wait()
	d.db.Close()
	d.db = nil
}
