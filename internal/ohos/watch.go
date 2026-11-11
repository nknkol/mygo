//go:build linux && (amd64 || arm64)

package ohos

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"syscall"

	"github.com/egoist/mygo/internal/platform"
	"github.com/egoist/mygo/internal/watchdriver"
)

// ReplaceFile renames replacement over target, preserving permissions and
// owner/group metadata where permitted.
func (*Backend) ReplaceFile(target, replacement string) error {
	if info, err := os.Stat(target); err == nil {
		if err := os.Chmod(replacement, info.Mode().Perm()|info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)); err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			_ = os.Lchown(replacement, int(st.Uid), int(st.Gid))
		}
	}
	return os.Rename(replacement, target)
}

type inotifyUser struct {
	watch *inotifyWatch
	id    uint64
}

type inotifyGeneration struct {
	users   []inotifyUser
	retired bool
}

type inotifyInstance struct {
	fd          int
	file        *os.File
	users       int
	generations map[int][]*inotifyGeneration
}

var inotifyShared struct {
	sync.Mutex
	instance *inotifyInstance
}

type inotifyWatch struct {
	instance *inotifyInstance
	targets  map[uint64]int
	mu       sync.Mutex
	queue    []platform.FileWatchNotice
	err      error
	signal   chan struct{}
}

func (*Backend) NewFileWatch(notify func(platform.FileWatchNotice)) (platform.FileWatch, error) {
	inotifyShared.Lock()
	defer inotifyShared.Unlock()
	in := inotifyShared.instance
	if in == nil {
		fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
		if err != nil {
			return nil, err
		}
		in = &inotifyInstance{fd: fd, file: os.NewFile(uintptr(fd), "inotify"), generations: make(map[int][]*inotifyGeneration)}
		inotifyShared.instance = in
		go in.read(in.file)
	}
	in.users++
	d := &inotifyWatch{instance: in, targets: make(map[uint64]int), signal: make(chan struct{}, 1)}
	return watchdriver.New(d, notify), nil
}

func (d *inotifyWatch) Add(t platform.FileWatchTarget) error {
	if t.Subtree || !t.Directory {
		return platform.ErrUnsupported
	}
	fd, err := syscall.Open(t.Path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), t.Path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, t.Info) {
		return errors.New("watch target identity changed")
	}
	inotifyShared.Lock()
	defer inotifyShared.Unlock()
	in := d.instance
	if in.file == nil {
		return errors.New("inotify instance closed")
	}
	mask := uint32(syscall.IN_CREATE | syscall.IN_MODIFY | syscall.IN_CLOSE_WRITE | syscall.IN_ATTRIB | syscall.IN_DELETE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO | syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF | syscall.IN_UNMOUNT | syscall.IN_ONLYDIR | syscall.IN_DONT_FOLLOW)
	wd, err := syscall.InotifyAddWatch(in.fd, t.Path, mask)
	if err != nil {
		return err
	}
	gens := in.generations[wd]
	info, err = os.Lstat(t.Path)
	if err != nil || !os.SameFile(info, t.Info) {
		if len(gens) == 0 || gens[len(gens)-1].retired {
			syscall.InotifyRmWatch(in.fd, uint32(wd))
		}
		if err != nil {
			return err
		}
		return errors.New("watch target identity changed")
	}
	if len(gens) == 0 || gens[len(gens)-1].retired {
		gens = append(gens, &inotifyGeneration{})
	}
	g := gens[len(gens)-1]
	g.users = append(g.users, inotifyUser{d, t.ID})
	in.generations[wd] = gens
	d.targets[t.ID] = wd
	return nil
}

func (d *inotifyWatch) Remove(id uint64) {
	inotifyShared.Lock()
	defer inotifyShared.Unlock()
	d.remove(id)
}

func (d *inotifyWatch) remove(id uint64) {
	wd, ok := d.targets[id]
	if !ok {
		return
	}
	delete(d.targets, id)
	in := d.instance
	for _, g := range in.generations[wd] {
		for i, u := range g.users {
			if u == (inotifyUser{d, id}) {
				g.users = append(g.users[:i], g.users[i+1:]...)
				if len(g.users) == 0 && !g.retired {
					g.retired = true
					if in.file != nil {
						syscall.InotifyRmWatch(in.fd, uint32(wd))
					}
				}
				return
			}
		}
	}
}

type inotifyRecord struct {
	wd           int
	mask, cookie uint32
	name         string
}

func parseInotify(buf []byte) ([]inotifyRecord, error) {
	var records []inotifyRecord
	for len(buf) > 0 {
		if len(buf) < 16 {
			return nil, errors.New("truncated inotify record")
		}
		length := uint64(binary.LittleEndian.Uint32(buf[12:16]))
		if length > uint64(len(buf)-16) {
			return nil, errors.New("oversized inotify name")
		}
		name := ""
		if length > 0 {
			data := buf[16 : 16+int(length)]
			end := bytes.IndexByte(data, 0)
			if end < 0 {
				return nil, errors.New("unterminated inotify name")
			}
			name = string(data[:end])
		}
		records = append(records, inotifyRecord{int(int32(binary.LittleEndian.Uint32(buf))), binary.LittleEndian.Uint32(buf[4:8]), binary.LittleEndian.Uint32(buf[8:12]), name})
		buf = buf[16+int(length):]
	}
	return records, nil
}

func (in *inotifyInstance) read(file *os.File) {
	var buf [65536]byte
	for {
		size, err := file.Read(buf[:])
		if err == nil && size == 0 {
			err = errors.New("inotify stream ended")
		}
		var records []inotifyRecord
		if err == nil {
			records, err = parseInotify(buf[:size])
		}
		inotifyShared.Lock()
		if err != nil {
			if in.file != nil {
				for w := range in.watches() {
					w.post(nil, err)
				}
				in.close()
			}
			inotifyShared.Unlock()
			return
		}
		notices := map[*inotifyWatch][]platform.FileWatchNotice{}
		for _, r := range records {
			in.dispatch(r, notices)
		}
		inotifyShared.Unlock()
		for w, n := range notices {
			w.post(n, nil)
		}
	}
}

func (in *inotifyInstance) watches() map[*inotifyWatch]bool {
	all := map[*inotifyWatch]bool{}
	for _, gens := range in.generations {
		for _, g := range gens {
			for _, u := range g.users {
				all[u.watch] = true
			}
		}
	}
	return all
}

func (in *inotifyInstance) dispatch(r inotifyRecord, notices map[*inotifyWatch][]platform.FileWatchNotice) {
	if r.mask&syscall.IN_Q_OVERFLOW != 0 {
		for w := range in.watches() {
			notices[w] = append(notices[w], platform.FileWatchNotice{Err: platform.ErrFileWatchOverflow})
		}
		return
	}
	gens := in.generations[r.wd]
	if len(gens) == 0 {
		return
	}
	g := gens[0]
	if r.mask&syscall.IN_IGNORED != 0 {
		if len(gens) == 1 {
			delete(in.generations, r.wd)
		} else {
			in.generations[r.wd] = gens[1:]
		}
		if !g.retired {
			for _, u := range g.users {
				notices[u.watch] = append(notices[u.watch], platform.FileWatchNotice{Target: u.id, Kind: platform.FileWatchLost})
				delete(u.watch.targets, u.id)
			}
		}
		return
	}
	if g.retired || len(g.users) == 0 {
		return
	}
	var kind platform.FileWatchKind
	switch {
	case r.mask&syscall.IN_UNMOUNT != 0:
		return
	case r.mask&syscall.IN_MOVED_FROM != 0:
		kind = platform.FileWatchMoveFrom
	case r.mask&syscall.IN_MOVED_TO != 0:
		kind = platform.FileWatchMoveTo
	case r.mask&syscall.IN_CREATE != 0:
		kind = platform.FileWatchCreate
	case r.mask&(syscall.IN_DELETE|syscall.IN_DELETE_SELF) != 0:
		kind = platform.FileWatchRemove
	case r.mask&(syscall.IN_MODIFY|syscall.IN_CLOSE_WRITE) != 0:
		kind = platform.FileWatchWrite
	}
	seen := map[*inotifyWatch]bool{}
	for i := len(g.users) - 1; i >= 0; i-- {
		u := g.users[i]
		if !seen[u.watch] {
			seen[u.watch] = true
			notices[u.watch] = append(notices[u.watch], platform.FileWatchNotice{Target: u.id, Kind: kind, Name: r.name})
		}
	}
	if r.mask&syscall.IN_DELETE_SELF != 0 {
		g.retired = true
	}
}

func (in *inotifyInstance) close() {
	if in.file != nil {
		in.file.Close()
		in.file = nil
	}
	if inotifyShared.instance == in {
		inotifyShared.instance = nil
	}
}

func (d *inotifyWatch) post(notices []platform.FileWatchNotice, err error) {
	d.mu.Lock()
	d.queue = append(d.queue, notices...)
	if err != nil && d.err == nil {
		d.err = err
	}
	d.mu.Unlock()
	d.Wake()
}

func (d *inotifyWatch) Wait() ([]platform.FileWatchNotice, error) {
	d.mu.Lock()
	if len(d.queue) == 0 && d.err == nil {
		d.mu.Unlock()
		<-d.signal
		d.mu.Lock()
	}
	notices, err := d.queue, d.err
	d.queue = nil
	d.mu.Unlock()
	return notices, err
}

func (d *inotifyWatch) Wake() {
	select {
	case d.signal <- struct{}{}:
	default:
	}
}

func (d *inotifyWatch) Close() {
	inotifyShared.Lock()
	defer inotifyShared.Unlock()
	for id := range d.targets {
		d.remove(id)
	}
	in := d.instance
	if in.users--; in.users == 0 {
		in.close()
	}
}
