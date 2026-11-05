// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"flag"
	"testing"
)

// The default has to be the run that writes nothing: an operator who forgets
// the flag gets a report, not a change across every live checklist.
func TestParseSyncRequiresWriterArgs(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantApply bool
		wantErr   bool
	}{
		{name: "no flags is a dry run", args: nil},
		{name: "--apply writes", args: []string{"--apply"}, wantApply: true},
		{name: "an unknown flag is refused", args: []string{"--aply"}, wantErr: true},
		{name: "a stray argument is refused", args: []string{"apply"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			apply, err := parseSyncRequiresWriterArgs(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if apply != tc.wantApply {
				t.Errorf("apply = %v, want %v", apply, tc.wantApply)
			}
		})
	}
}

// The command treats help as success and prints usage, which needs the
// parser's help error to stay recognisable through the wrapping.
func TestParseSyncRequiresWriterArgsReportsHelp(t *testing.T) {
	if _, err := parseSyncRequiresWriterArgs([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("parse(-h) = %v, want flag.ErrHelp", err)
	}
}
