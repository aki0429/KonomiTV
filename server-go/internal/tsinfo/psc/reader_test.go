package psc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
)

func TestMatchesPinnedSource(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name      string
			Input     []byte
			PIDs      []uint16
			StopAfter int `json:"stop_after"`
			Valid     bool
			Sections  []Section
		}
	}
	data, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			sections := []Section{}
			valid, err := Read(context.Background(), bytes.NewReader(tc.Input), tc.PIDs, func(s Section) bool {
				sections = append(sections, s)
				return tc.StopAfter == 0 || len(sections) < tc.StopAfter
			})
			if err != nil || valid != tc.Valid || !reflect.DeepEqual(sections, tc.Sections) {
				t.Fatalf("valid=%v error=%v sections=%v want valid=%v sections=%v", valid, err, sections, tc.Valid, tc.Sections)
			}
		})
	}
}

type brokenReader struct{ err error }

func (r brokenReader) Read([]byte) (int, error) { return 0, r.err }

func TestReaderCancellationAndIOFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, bytes.NewReader(nil), nil, func(Section) bool { return true }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	failure := errors.New("storage failure")
	if _, err := Read(context.Background(), brokenReader{failure}, nil, func(Section) bool { return true }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if valid, err := Read(context.Background(), brokenReader{io.EOF}, nil, func(Section) bool { return true }); err != nil || !valid {
		t.Fatalf("valid=%v error=%v", valid, err)
	}
}
