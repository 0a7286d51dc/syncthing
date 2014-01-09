package model

/*

Locking
=======

The model has read and write locks. These must be acquired as appropriate by
public methods. To prevent deadlock situations, private methods should never
acquire locks, but document what locks they require.

*/

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"sync"
	"time"

	"github.com/calmh/syncthing/buffers"
	"github.com/calmh/syncthing/protocol"
)

type Model struct {
	fieldLock sync.RWMutex // protects all model fields
	walkLock  sync.Mutex   // exclusion between pulling and walking for updates

	dir string

	global map[string]File            // the latest version of each file as it exists in the cluster
	local  map[string]File            // the files we currently have locally on disk
	remote map[string]map[string]File // the remote indexes
	need   map[string]bool            // the files we need to update

	nodes   map[string]*protocol.Connection // the protocol connection per node
	rawConn map[string]io.ReadWriteCloser   // the underlying connection object per node

	updatedLocal int64 // timestamp of last update to local
	updateGlobal int64 // timestamp of last update to remote

	lastIdxBcast        time.Time // last time we sent an index
	lastIdxBcastRequest time.Time // last time someone wanted to send and index

	rwRunning      bool // we have started r/w processing
	delete         bool // we are allowed to delete files
	parallellFiles int  // maximum number of files to pull in parallell
	paralllelReqs  int  // maximum number of parallell requests per file

	trace map[string]bool // the set of enabled trace (debugging) categories

	fileLastChanged   map[string]time.Time // last time we updated a file in the index
	fileWasSuppressed map[string]int       // how many update rounds we have suppressed changes to the file
}

const (
	idxBcastHoldtime = 15 * time.Second  // Wait at least this long after the last index modification
	idxBcastMaxDelay = 120 * time.Second // Unless we've already waited this long

	minFileHoldTimeS = 60  // Never allow file changes more often than this
	maxFileHoldTimeS = 600 // Always allow file changes at least this often
)

var (
	ErrNoSuchFile = errors.New("no such file")
	ErrInvalid    = errors.New("file is invalid")
)

// NewModel creates and starts a new model. The model starts in read-only mode,
// where it sends index information to connected peers and responds to requests
// for file data without altering the local repository in any way.
func NewModel(dir string) *Model {
	m := &Model{
		dir:               dir,
		global:            make(map[string]File),
		local:             make(map[string]File),
		remote:            make(map[string]map[string]File),
		need:              make(map[string]bool),
		nodes:             make(map[string]*protocol.Connection),
		rawConn:           make(map[string]io.ReadWriteCloser),
		lastIdxBcast:      time.Now(),
		trace:             make(map[string]bool),
		fileLastChanged:   make(map[string]time.Time),
		fileWasSuppressed: make(map[string]int),
	}

	go m.broadcastIndexLoop()
	return m
}

// Trace enables trace logging of the given facility. This is a debugging function; grep for m.trace.
func (m *Model) Trace(t string) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()
	m.trace[t] = true
}

// StartRW starts read/write processing on the current model. When in
// read/write mode the model will attempt to keep in sync with the cluster by
// pulling needed files from peer nodes.
func (m *Model) StartRW(del bool, pfiles, preqs int) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()

	if m.rwRunning {
		panic("starting started model")
	}

	m.rwRunning = true
	m.delete = del
	m.parallellFiles = pfiles
	m.paralllelReqs = preqs

	go m.cleanTempFiles()
	go m.puller()
}

// Generation returns an opaque integer that is guaranteed to increment on
// every change to the local repository or global model.
func (m *Model) Generation() int64 {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()

	return m.updatedLocal + m.updateGlobal
}

type ConnectionInfo struct {
	protocol.Statistics
	Address string
}

// ConnectionStats returns a map with connection statistics for each connected node.
func (m *Model) ConnectionStats() map[string]ConnectionInfo {
	type remoteAddrer interface {
		RemoteAddr() net.Addr
	}

	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()

	var res = make(map[string]ConnectionInfo)
	for node, conn := range m.nodes {
		ci := ConnectionInfo{
			Statistics: conn.Statistics(),
		}
		if nc, ok := m.rawConn[node].(remoteAddrer); ok {
			ci.Address = nc.RemoteAddr().String()
		}
		res[node] = ci
	}
	return res
}

// LocalSize returns the number of files, deleted files and total bytes for all
// files in the global model.
func (m *Model) GlobalSize() (files, deleted, bytes int) {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()

	for _, f := range m.global {
		if f.Flags&protocol.FlagDeleted == 0 {
			files++
			bytes += f.Size()
		} else {
			deleted++
		}
	}
	return
}

// LocalSize returns the number of files, deleted files and total bytes for all
// files in the local repository.
func (m *Model) LocalSize() (files, deleted, bytes int) {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()

	for _, f := range m.local {
		if f.Flags&protocol.FlagDeleted == 0 {
			files++
			bytes += f.Size()
		} else {
			deleted++
		}
	}
	return
}

// InSyncSize returns the number and total byte size of the local files that
// are in sync with the global model.
func (m *Model) InSyncSize() (files, bytes int) {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()

	for n, f := range m.local {
		if gf, ok := m.global[n]; ok && f.Equals(gf) {
			files++
			bytes += f.Size()
		}
	}
	return
}

// NeedFiles returns the list of currently needed files and the total size.
func (m *Model) NeedFiles() (files []File, bytes int) {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()

	for n := range m.need {
		f := m.global[n]
		files = append(files, f)
		bytes += f.Size()
	}
	return
}

// Index is called when a new node is connected and we receive their full index.
// Implements the protocol.Model interface.
func (m *Model) Index(nodeID string, fs []protocol.FileInfo) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()

	if m.trace["net"] {
		log.Printf("NET IDX(in): %s: %d files", nodeID, len(fs))
	}

	m.remote[nodeID] = make(map[string]File)
	for _, f := range fs {
		m.remote[nodeID][f.Name] = fileFromFileInfo(f)
		if m.trace["idx"] {
			var flagComment string
			if f.Flags&protocol.FlagDeleted != 0 {
				flagComment = " (deleted)"
			}
			log.Printf("IDX(in): %q m=%d f=%o%s v=%d (%d blocks)", f.Name, f.Modified, f.Flags, flagComment, f.Version, len(f.Blocks))
		}
	}

	m.recomputeGlobal()
	m.recomputeNeed()
}

// IndexUpdate is called for incremental updates to connected nodes' indexes.
// Implements the protocol.Model interface.
func (m *Model) IndexUpdate(nodeID string, fs []protocol.FileInfo) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()

	if m.trace["net"] {
		log.Printf("NET IDXUP(in): %s: %d files", nodeID, len(fs))
	}

	repo, ok := m.remote[nodeID]
	if !ok {
		return
	}

	for _, f := range fs {
		repo[f.Name] = fileFromFileInfo(f)
		if m.trace["idx"] {
			var flagComment string
			if f.Flags&protocol.FlagDeleted != 0 {
				flagComment = " (deleted)"
			}
			log.Printf("IDX(in-up): %q m=%d f=%o%s v=%d (%d blocks)", f.Name, f.Modified, f.Flags, flagComment, f.Version, len(f.Blocks))
		}
	}

	m.recomputeGlobal()
	m.recomputeNeed()
}

// Close removes the peer from the model and closes the underlyign connection if possible.
// Implements the protocol.Model interface.
func (m *Model) Close(node string, err error) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()

	conn, ok := m.rawConn[node]
	if ok {
		conn.Close()
	}

	delete(m.remote, node)
	delete(m.nodes, node)
	delete(m.rawConn, node)

	m.recomputeGlobal()
	m.recomputeNeed()
}

// Request returns the specified data segment by reading it from local disk.
// Implements the protocol.Model interface.
func (m *Model) Request(nodeID, name string, offset uint64, size uint32, hash []byte) ([]byte, error) {
	// Verify that the requested file exists in the local and global model.
	m.fieldLock.RLock()
	lf, localOk := m.local[name]
	_, globalOk := m.global[name]
	m.fieldLock.RUnlock()
	if !localOk || !globalOk {
		log.Printf("SECURITY (nonexistent file) REQ(in): %s: %q o=%d s=%d h=%x", nodeID, name, offset, size, hash)
		return nil, ErrNoSuchFile
	}
	if lf.Flags&protocol.FlagInvalid != 0 {
		return nil, ErrInvalid
	}

	if m.trace["net"] && nodeID != "<local>" {
		log.Printf("NET REQ(in): %s: %q o=%d s=%d h=%x", nodeID, name, offset, size, hash)
	}
	fn := path.Join(m.dir, name)
	fd, err := os.Open(fn) // XXX: Inefficient, should cache fd?
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	buf := buffers.Get(int(size))
	_, err = fd.ReadAt(buf, int64(offset))
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// ReplaceLocal replaces the local repository index with the given list of files.
// Change suppression is applied to files changing too often.
func (m *Model) ReplaceLocal(fs []File) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()

	var updated bool
	var newLocal = make(map[string]File)

	for _, f := range fs {
		newLocal[f.Name] = f
		if ef := m.local[f.Name]; !ef.Equals(f) {
			updated = true
		}
	}

	if m.markDeletedLocals(newLocal) {
		updated = true
	}

	if len(newLocal) != len(m.local) {
		updated = true
	}

	if updated {
		m.local = newLocal
		m.recomputeGlobal()
		m.recomputeNeed()
		m.updatedLocal = time.Now().Unix()
		m.lastIdxBcastRequest = time.Now()
	}
}

// SeedLocal replaces the local repository index with the given list of files,
// in protocol data types. Does not track deletes, should only be used to seed
// the local index from a cache file at startup.
func (m *Model) SeedLocal(fs []protocol.FileInfo) {
	m.fieldLock.Lock()
	defer m.fieldLock.Unlock()

	m.local = make(map[string]File)
	for _, f := range fs {
		m.local[f.Name] = fileFromFileInfo(f)
	}

	m.recomputeGlobal()
	m.recomputeNeed()
}

// ConnectedTo returns true if we are connected to the named node.
func (m *Model) ConnectedTo(nodeID string) bool {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()
	_, ok := m.nodes[nodeID]
	return ok
}

// ProtocolIndex returns the current local index in protocol data types.
func (m *Model) ProtocolIndex() []protocol.FileInfo {
	m.fieldLock.RLock()
	defer m.fieldLock.RUnlock()
	return m.protocolIndex()
}

// RepoID returns a unique ID representing the current repository location.
func (m *Model) RepoID() string {
	return fmt.Sprintf("%x", sha1.Sum([]byte(m.dir)))
}

// AddConnection adds a new peer connection to the model. An initial index will
// be sent to the connected peer, thereafter index updates whenever the local
// repository changes.
func (m *Model) AddConnection(conn io.ReadWriteCloser, nodeID string) {
	node := protocol.NewConnection(nodeID, conn, conn, m)

	m.fieldLock.Lock()
	m.nodes[nodeID] = node
	m.rawConn[nodeID] = conn
	m.fieldLock.Unlock()

	m.fieldLock.RLock()
	idx := m.protocolIndex()
	m.fieldLock.RUnlock()

	go func() {
		node.Index(idx)
	}()
}

func (m *Model) shouldSuppressChange(name string) bool {
	sup := shouldSuppressChange(m.fileLastChanged[name], m.fileWasSuppressed[name])
	if sup {
		m.fileWasSuppressed[name]++
	} else {
		m.fileWasSuppressed[name] = 0
		m.fileLastChanged[name] = time.Now()
	}
	return sup
}

func shouldSuppressChange(lastChange time.Time, numChanges int) bool {
	sinceLast := time.Since(lastChange)
	if sinceLast > maxFileHoldTimeS*time.Second {
		return false
	}
	if sinceLast < time.Duration((numChanges+2)*minFileHoldTimeS)*time.Second {
		return true
	}
	return false
}

// protocolIndex returns the current local index in protocol data types.
// Must be called with the read lock held.
func (m *Model) protocolIndex() []protocol.FileInfo {
	var index []protocol.FileInfo
	for _, f := range m.local {
		mf := fileInfoFromFile(f)
		if m.trace["idx"] {
			var flagComment string
			if mf.Flags&protocol.FlagDeleted != 0 {
				flagComment = " (deleted)"
			}
			log.Printf("IDX(out): %q m=%d f=%o%s v=%d (%d blocks)", mf.Name, mf.Modified, mf.Flags, flagComment, mf.Version, len(mf.Blocks))
		}
		index = append(index, mf)
	}
	return index
}

func (m *Model) requestGlobal(nodeID, name string, offset uint64, size uint32, hash []byte) ([]byte, error) {
	m.fieldLock.RLock()
	nc, ok := m.nodes[nodeID]
	m.fieldLock.RUnlock()
	if !ok {
		return nil, fmt.Errorf("requestGlobal: no such node: %s", nodeID)
	}

	if m.trace["net"] {
		log.Printf("NET REQ(out): %s: %q o=%d s=%d h=%x", nodeID, name, offset, size, hash)
	}

	return nc.Request(name, offset, size, hash)
}

func (m *Model) broadcastIndexLoop() {
	for {
		m.fieldLock.RLock()
		bcastRequested := m.lastIdxBcastRequest.After(m.lastIdxBcast)
		holdtimeExceeded := time.Since(m.lastIdxBcastRequest) > idxBcastHoldtime
		m.fieldLock.RUnlock()

		maxDelayExceeded := time.Since(m.lastIdxBcast) > idxBcastMaxDelay
		if bcastRequested && (holdtimeExceeded || maxDelayExceeded) {
			m.fieldLock.Lock()
			var indexWg sync.WaitGroup
			indexWg.Add(len(m.nodes))
			idx := m.protocolIndex()
			m.lastIdxBcast = time.Now()
			for _, node := range m.nodes {
				node := node
				if m.trace["net"] {
					log.Printf("NET IDX(out/loop): %s: %d files", node.ID, len(idx))
				}
				go func() {
					node.Index(idx)
					indexWg.Done()
				}()
			}
			m.fieldLock.Unlock()
			indexWg.Wait()
		}
		time.Sleep(idxBcastHoldtime)
	}
}

// markDeletedLocals sets the deleted flag on files that have gone missing locally.
// Must be called with the write lock held.
func (m *Model) markDeletedLocals(newLocal map[string]File) bool {
	// For every file in the existing local table, check if they are also
	// present in the new local table. If they are not, check that we already
	// had the newest version available according to the global table and if so
	// note the file as having been deleted.
	var updated bool
	for n, f := range m.local {
		if _, ok := newLocal[n]; !ok {
			if gf := m.global[n]; !gf.NewerThan(f) {
				if f.Flags&protocol.FlagDeleted == 0 {
					f.Flags = protocol.FlagDeleted
					f.Version++
					f.Blocks = nil
					updated = true
				}
				newLocal[n] = f
			}
		}
	}
	return updated
}

func (m *Model) updateLocal(f File) {
	if ef, ok := m.local[f.Name]; !ok || !ef.Equals(f) {
		m.local[f.Name] = f
		m.recomputeGlobal()
		m.recomputeNeed()
		m.updatedLocal = time.Now().Unix()
		m.lastIdxBcastRequest = time.Now()
	}
}

// Must be called with the write lock held.
func (m *Model) recomputeGlobal() {
	var newGlobal = make(map[string]File)

	for n, f := range m.local {
		newGlobal[n] = f
	}

	for _, fs := range m.remote {
		for n, nf := range fs {
			if lf, ok := newGlobal[n]; !ok || nf.NewerThan(lf) {
				newGlobal[n] = nf
			}
		}
	}

	// Figure out if anything actually changed

	var updated bool
	if len(newGlobal) != len(m.global) {
		updated = true
	} else {
		for n, f0 := range newGlobal {
			if f1, ok := m.global[n]; !ok || !f0.Equals(f1) {
				updated = true
				break
			}
		}
	}

	if updated {
		m.updateGlobal = time.Now().Unix()
		m.global = newGlobal
	}
}

// Must be called with the write lock held.
func (m *Model) recomputeNeed() {
	m.need = make(map[string]bool)
	for n, gf := range m.global {
		lf, ok := m.local[n]
		if !ok || gf.NewerThan(lf) {
			if gf.Flags&protocol.FlagInvalid != 0 {
				// Never attempt to sync invalid files
				continue
			}
			if gf.Flags&protocol.FlagDeleted != 0 && !m.delete {
				// Don't want to delete files, so forget this need
				continue
			}
			if gf.Flags&protocol.FlagDeleted != 0 && !ok {
				// Don't have the file, so don't need to delete it
				continue
			}
			if m.trace["need"] {
				log.Println("NEED:", ok, lf, gf)
			}
			m.need[n] = true
		}
	}
}

// Must be called with the read lock held.
func (m *Model) whoHas(name string) []string {
	var remote []string

	gf := m.global[name]
	for node, files := range m.remote {
		if file, ok := files[name]; ok && file.Equals(gf) {
			remote = append(remote, node)
		}
	}

	return remote
}

func fileFromFileInfo(f protocol.FileInfo) File {
	var blocks []Block
	var offset uint64
	for _, b := range f.Blocks {
		blocks = append(blocks, Block{
			Offset: offset,
			Length: b.Length,
			Hash:   b.Hash,
		})
		offset += uint64(b.Length)
	}
	return File{
		Name:     f.Name,
		Flags:    f.Flags,
		Modified: int64(f.Modified),
		Version:  f.Version,
		Blocks:   blocks,
	}
}

func fileInfoFromFile(f File) protocol.FileInfo {
	var blocks []protocol.BlockInfo
	for _, b := range f.Blocks {
		blocks = append(blocks, protocol.BlockInfo{
			Length: b.Length,
			Hash:   b.Hash,
		})
	}
	return protocol.FileInfo{
		Name:     f.Name,
		Flags:    f.Flags,
		Modified: int64(f.Modified),
		Version:  f.Version,
		Blocks:   blocks,
	}
}
