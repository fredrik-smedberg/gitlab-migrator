package main

import (
	"testing"
	"time"

	"github.com/xanzy/go-gitlab"
)

func TestSortCommitsByDateHandlesNil(t *testing.T) {
	early := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	commits := []*gitlab.Commit{
		{ID: "late", CommittedDate: &late},
		{ID: "nil", CommittedDate: nil},
		{ID: "early", CommittedDate: &early},
	}

	sortCommitsByDate(commits)

	want := []string{"nil", "early", "late"}
	for i, id := range want {
		if commits[i].ID != id {
			t.Fatalf("position %d: got %s, want %s", i, commits[i].ID, id)
		}
	}
}
