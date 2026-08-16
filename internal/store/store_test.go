package store

import (
	"testing"
	"time"

	"github.com/wjzhangq/gpumon/internal/model"
)

func snap(node string, i int) model.Snapshot {
	return model.Snapshot{
		Node:      node,
		Timestamp: time.Unix(int64(i), 0),
		Online:    true,
	}
}

func TestRingKeepsNewestAndOrdersOldestFirst(t *testing.T) {
	s := New(3, []string{"a"})
	for i := 1; i <= 5; i++ {
		s.Append(snap("a", i))
	}

	got, ok := s.History("a", 0)
	if !ok {
		t.Fatal("节点 a 应存在")
	}
	if len(got) != 3 {
		t.Fatalf("环形缓冲应只留 3 条，得到 %d", len(got))
	}
	for i, want := range []int64{3, 4, 5} {
		if got[i].Timestamp.Unix() != want {
			t.Fatalf("第 %d 条应为 %d，得到 %d", i, want, got[i].Timestamp.Unix())
		}
	}

	latest, ok := s.Latest("a")
	if !ok || latest.Timestamp.Unix() != 5 {
		t.Fatalf("最新一条错误: %#v", latest)
	}
}

func TestHistoryLimit(t *testing.T) {
	s := New(10, []string{"a"})
	for i := 1; i <= 10; i++ {
		s.Append(snap("a", i))
	}

	got, _ := s.History("a", 2)
	if len(got) != 2 {
		t.Fatalf("limit 未生效: %d", len(got))
	}
	if got[0].Timestamp.Unix() != 9 || got[1].Timestamp.Unix() != 10 {
		t.Fatalf("limit 应返回最新的 N 条: %v %v", got[0].Timestamp.Unix(), got[1].Timestamp.Unix())
	}
}

func TestEmptyAndUnknownNode(t *testing.T) {
	s := New(5, []string{"a"})

	if _, ok := s.Latest("a"); ok {
		t.Fatal("尚无数据时不应返回快照")
	}
	got, ok := s.History("a", 0)
	if !ok || len(got) != 0 {
		t.Fatalf("空节点应返回空切片: ok=%v len=%d", ok, len(got))
	}
	if _, ok := s.History("nope", 0); ok {
		t.Fatal("未知节点应返回 false")
	}
	if s.Has("nope") {
		t.Fatal("Has 对未知节点应为 false")
	}
}

func TestAppendCreatesUnknownNode(t *testing.T) {
	s := New(2, nil)
	s.Append(snap("dynamic", 1))

	if !s.Has("dynamic") {
		t.Fatal("Append 应自动登记新节点")
	}
	names := s.Names()
	if len(names) != 1 || names[0] != "dynamic" {
		t.Fatalf("节点顺序错误: %#v", names)
	}
}
