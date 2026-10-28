package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/xanzy/go-gitlab"
)

func TestPrepareMergeRequestsRecoversPanicPerMergeRequest(t *testing.T) {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	defer func(old int) { mergeRequestConcurrency = old }(mergeRequestConcurrency)
	mergeRequestConcurrency = 3

	mergeRequests := make([]*gitlab.MergeRequest, 10)
	for i := range mergeRequests {
		mergeRequests[i] = &gitlab.MergeRequest{IID: i + 1}
	}

	// A nil index makes prepareMergeRequest panic on its first line.
	p := &project{}
	work, errs := p.prepareMergeRequests(context.Background(), mergeRequests, nil)

	if len(work) != len(mergeRequests) || len(errs) != len(mergeRequests) {
		t.Fatalf("got %d results and %d errors, want %d each", len(work), len(errs), len(mergeRequests))
	}
	for i, err := range errs {
		if work[i] != nil {
			t.Errorf("index %d: got work, want nil", i)
		}
		want := fmt.Sprintf("merge request %d: panic:", i+1)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("index %d: error %v, want prefix %q", i, err, want)
		}
	}
}

func TestPrepareMergeRequestsCanceledStartsNothing(t *testing.T) {
	if logger == nil {
		logger = hclog.NewNullLogger()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := &project{}
	work, errs := p.prepareMergeRequests(ctx, []*gitlab.MergeRequest{{IID: 1}, {IID: 2}}, nil)
	for i := range work {
		if work[i] != nil || errs[i] != nil {
			t.Errorf("index %d: got work %v, error %v; want neither", i, work[i], errs[i])
		}
	}
}
