package main

import "testing"

func TestMergeRequestIIDFromBody(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		want   int
		wantOK bool
	}{
		{name: "linked", body: "| **GitLab MR Number** | [17](https://x/merge_requests/17) |", want: 17, wantOK: true},
		{name: "plain", body: "| **GitLab MR Number** | 17 |", want: 17, wantOK: true},
		{name: "empty", body: "", wantOK: false},
		{name: "longer number", body: "| **GitLab MR Number** | [170]", want: 170, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := mergeRequestIIDFromBody(tt.body)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("mergeRequestIIDFromBody(%q) = (%d, %t), want (%d, %t)", tt.body, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestIssueNumberFromURL(t *testing.T) {
	tests := []struct {
		url    string
		want   int
		wantOK bool
	}{
		{url: "https://api.github.com/repos/o/r/issues/42", want: 42, wantOK: true},
		{url: "garbage", wantOK: false},
	}

	for _, tt := range tests {
		got, ok := issueNumberFromURL(tt.url)
		if ok != tt.wantOK || got != tt.want {
			t.Fatalf("issueNumberFromURL(%q) = (%d, %t), want (%d, %t)", tt.url, got, ok, tt.want, tt.wantOK)
		}
	}
}
