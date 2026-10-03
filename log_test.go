package main

import (
	"fmt"
	"testing"
)

func TestLogRingWrapOrderAndSnapshot(t *testing.T) {
	b := &LogRingBuffer{maxSize: 3}
	for i := range 10 {
		b.Push("INFO", fmt.Sprint(i))
	}
	for _, limit := range []int{-1, 0, 1, 2, 3, 5} {
		got := b.GetEntries(limit)
		n := limit
		if n <= 0 || n > 3 {
			n = 3
		}
		if len(got) != n {
			t.Fatal("wrong snapshot size")
		}
		for i, item := range got {
			if item.Message != fmt.Sprint(10-n+i) {
				t.Fatalf("order: %+v", got)
			}
		}
	}
	snapshot := b.GetEntries(0)
	b.Push("INFO", "10")
	if snapshot[0].Message != "7" {
		t.Fatal("snapshot changed after overwrite")
	}
	if len(b.entries) != 3 || cap(b.entries) > 4 {
		t.Fatal("unbounded buffer")
	}
}

func TestLogRingPartialAndEmpty(t *testing.T) {
	b := &LogRingBuffer{maxSize: 3}
	if len(b.GetEntries(0)) != 0 {
		t.Fatal("nonempty initial buffer")
	}
	b.Push("INFO", "one")
	b.Push("INFO", "two")
	if got := b.GetEntries(1); len(got) != 1 || got[0].Message != "two" {
		t.Fatal(got)
	}
	zero := &LogRingBuffer{}
	zero.Push("INFO", "discard")
	if len(zero.GetEntries(0)) != 0 {
		t.Fatal("zero capacity")
	}
}
