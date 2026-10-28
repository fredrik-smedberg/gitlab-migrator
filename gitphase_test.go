package main

import (
	"reflect"
	"testing"
)

func TestChunk(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}

	got := chunk(items, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunk(items, 2) = %v, want %v", got, want)
	}

	if got := chunk([]string{}, 2); len(got) != 0 {
		t.Errorf("chunk(empty, 2) = %v, want empty", got)
	}

	got = chunk(items, 0)
	want = [][]string{{"a", "b", "c", "d", "e"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunk(items, 0) = %v, want %v", got, want)
	}
}
