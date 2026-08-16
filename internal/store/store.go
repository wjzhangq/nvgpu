// Package store 提供纯内存的定长环形缓冲，保存每个节点最近 N 个快照。
//
// 按需求：不落盘，进程重启后历史清空，N 上限 60。
package store

import (
	"sync"

	"github.com/wjzhangq/gpumon/internal/model"
)

type ring struct {
	buf   []model.Snapshot
	start int // 最旧元素的下标
	count int
}

func newRing(size int) *ring {
	if size < 1 {
		size = 1
	}
	return &ring{buf: make([]model.Snapshot, size)}
}

func (r *ring) push(s model.Snapshot) {
	n := len(r.buf)
	if r.count < n {
		r.buf[(r.start+r.count)%n] = s
		r.count++
		return
	}
	// 满了：覆盖最旧的一个，并把 start 前移。
	r.buf[r.start] = s
	r.start = (r.start + 1) % n
}

// latest 返回最新一个快照。
func (r *ring) latest() (model.Snapshot, bool) {
	if r.count == 0 {
		return model.Snapshot{}, false
	}
	n := len(r.buf)
	return r.buf[(r.start+r.count-1)%n], true
}

// slice 返回最近 limit 个快照，按时间从旧到新排列。limit <= 0 表示全部。
func (r *ring) slice(limit int) []model.Snapshot {
	if r.count == 0 {
		return []model.Snapshot{}
	}
	take := r.count
	if limit > 0 && limit < take {
		take = limit
	}
	skip := r.count - take
	n := len(r.buf)
	out := make([]model.Snapshot, 0, take)
	for i := 0; i < take; i++ {
		out = append(out, r.buf[(r.start+skip+i)%n])
	}
	return out
}

// Store 是所有节点历史数据的容器，并发安全。
type Store struct {
	mu    sync.RWMutex
	size  int
	rings map[string]*ring
	order []string
}

// New 创建 Store。names 决定 API 输出时的节点顺序（即配置文件顺序）。
func New(size int, names []string) *Store {
	if size < 1 {
		size = 1
	}
	s := &Store{
		size:  size,
		rings: make(map[string]*ring, len(names)),
		order: append([]string(nil), names...),
	}
	for _, n := range names {
		s.rings[n] = newRing(size)
	}
	return s
}

// Size 返回每个节点保留的最大历史点数。
func (s *Store) Size() int { return s.size }

// Names 按配置顺序返回节点名。
func (s *Store) Names() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// Has 判断节点是否存在。
func (s *Store) Has(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.rings[name]
	return ok
}

// Append 追加一个快照。未在 New 中登记的节点会被动态创建。
func (s *Store) Append(snap model.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rings[snap.Node]
	if !ok {
		r = newRing(s.size)
		s.rings[snap.Node] = r
		s.order = append(s.order, snap.Node)
	}
	r.push(snap)
}

// Latest 返回某节点的最新快照。
func (s *Store) Latest(name string) (model.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rings[name]
	if !ok {
		return model.Snapshot{}, false
	}
	return r.latest()
}

// History 返回某节点最近 limit 个快照（从旧到新）。
func (s *Store) History(name string, limit int) ([]model.Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rings[name]
	if !ok {
		return nil, false
	}
	return r.slice(limit), true
}
