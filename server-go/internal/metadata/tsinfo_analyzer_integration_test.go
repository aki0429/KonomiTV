package metadata

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/tsinfo"
)

// This opt-in test reads private files. No broadcast bytes, title, path or EPG
// values are emitted or checked into the repository. The DB URI must explicitly
// request read-only SQLite mode; artifacts can only be written to the directory
// explicitly supplied by the operator.
func TestTSInfoPrivateOracle(t *testing.T) {
	root := os.Getenv("TSINFO_PARITY_ROOT")
	if root == "" {
		t.Skip("private Python-oracle fixture is opt-in")
	}
	uri := os.Getenv("TSINFO_CHANNEL_DB")
	if !strings.Contains(uri, "mode=ro") {
		t.Fatal("TSINFO_CHANNEL_DB must specify mode=ro")
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal("read-only DB unavailable")
	}
	results := []map[string]any{}
	for _, category := range []string{"gr_nhk", "gr_commercial_gaiji", "bs"} {
		t.Run(category, func(t *testing.T) {
			read := func(name string) map[string]any {
				raw, err := os.ReadFile(filepath.Join(root, category, name))
				if err != nil {
					t.Fatal("private artifact unavailable")
				}
				var v map[string]any
				if err := json.Unmarshal(raw, &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
			manifest, reference := read("manifest.json"), read("reference.json")
			input := manifest["selection_input"].(map[string]any)["recorded_video"].(map[string]any)
			converted := map[string]any{}
			for k, v := range input {
				parts := strings.Split(k, "_")
				for i := range parts {
					parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
				}
				if text, ok := v.(string); ok && (strings.HasSuffix(k, "_at") || strings.HasSuffix(k, "_time")) {
					v = strings.Replace(text, " ", "T", 1)
				}
				converted[strings.Join(parts, "")] = v
			}
			raw, _ := json.Marshal(converted)
			var video RecordedVideo
			if err := json.Unmarshal(raw, &video); err != nil {
				t.Fatal(err)
			}
			video.FilePath = manifest["source_file_path"].(string)
			video.Duration = reference["metadata_pcr"].(map[string]any)["duration_seconds"].(float64)
			// Assert independently inferred timestamps, not copied DB input.
			video.RecordingStartTime, video.RecordingEndTime = nil, nil
			end := int64(reference["metadata_pcr"].(map[string]any)["end_ts_offset"].(float64))
			sid := int(reference["conditions"].(map[string]any)["preferred_service_id"].(float64))
			before, err := os.Stat(video.FilePath)
			if err != nil {
				t.Fatal("recording unavailable")
			}
			started := time.Now()
			p, ok := NewTSInfoProgramAnalyzer(db, testLogger(t)).AnalyzeProgramContext(context.Background(), &video, &end, &sid)
			duration := time.Since(started).Seconds()
			after, err := os.Stat(video.FilePath)
			if err != nil {
				t.Fatal("recording stat failed")
			}
			checks := map[string]bool{"analyzed": ok && p != nil, "source_stat_unchanged": before.Size() == after.Size() && before.ModTime() == after.ModTime()}
			oracle := reference["oracle"].(map[string]any)
			if p != nil {
				actual := tsinfoOracleView(p, &video)
				for group, want := range oracle {
					for field, value := range want.(map[string]any) {
						checks[group+"."+field] = tsinfoJSONEqual(actual[group][field], value)
					}
				}
			}
			diagnostics := map[string]any{"timestamp_tolerance_seconds": 0.000001}
			if video.RecordingStartTime != nil {
				if clock, err := time.Parse(time.RFC3339Nano, oracle["recording"].(map[string]any)["recording_start_time"].(string)); err == nil {
					diagnostics["recording_start_delta_seconds"] = video.RecordingStartTime.Sub(clock).Seconds()
				}
			}
			results = append(results, map[string]any{"category": category, "checks": checks, "elapsed_seconds": duration, "diagnostics": diagnostics})
			mismatches := []string{}
			for k, v := range checks {
				if !v {
					mismatches = append(mismatches, k)
				}
			}
			if len(mismatches) > 0 {
				t.Errorf("private oracle mismatched fields: %v", mismatches)
			}
		})
	}
	if out := os.Getenv("TSINFO_PARITY_OUTPUT"); out != "" {
		raw, err := json.MarshalIndent(map[string]any{"count": len(results), "results": results}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func tsinfoJSONEqual(a, b any) bool {
	// Normalize Go nil pointers, integer types and structs to JSON values.
	raw, err := json.Marshal(a)
	if err != nil {
		return false
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	if x, ok := v.(string); ok {
		if y, ok := b.(string); ok {
			tx, ex := time.Parse(time.RFC3339Nano, x)
			ty, ey := time.Parse(time.RFC3339Nano, y)
			if ex == nil && ey == nil {
				return tx.Sub(ty).Abs() <= time.Microsecond
			}
		}
	}
	return reflect.DeepEqual(v, b)
}

func tsinfoOracleView(p *RecordedProgram, video *RecordedVideo) map[string]map[string]any {
	c := p.Channel
	detail := map[string]string{}
	for _, entry := range p.Detail {
		detail[entry.Key] = entry.Value
	}
	genres := []map[string]string{}
	for _, g := range p.Genres {
		genres = append(genres, map[string]string{"major": g.Major, "middle": g.Middle})
	}
	return map[string]map[string]any{
		"channel": {"id": c.ID, "display_channel_id": c.DisplayChannelID, "network_id": c.NetworkID, "service_id": c.ServiceID, "transport_stream_id": c.TransportStreamID, "remocon_id": c.RemoconID, "channel_number": c.ChannelNumber, "type": c.Type, "name": c.Name, "terrestrial_regions": nil, "jikkyo_force": c.JikkyoForce, "is_subchannel": c.IsSubchannel, "is_radiochannel": c.IsRadiochannel, "is_watchable": c.IsWatchable},
		"region": {"id": func() any {
			v := tsinfo.GetRegionIDFromNetworkID(c.NetworkID)
			if v == 0 {
				return nil
			}
			return v
		}(), "names": tsinfo.GetRegionNamesFromNetworkID(c.NetworkID)},
		"program":   {"network_id": p.NetworkID, "service_id": p.ServiceID, "event_id": p.EventID, "title": p.Title, "description": p.Description, "detail": detail, "start_time": p.StartTime, "end_time": p.EndTime, "duration": p.Duration, "is_free": p.IsFree, "genres": genres, "primary_audio_type": p.PrimaryAudioType, "primary_audio_language": p.PrimaryAudioLanguage, "secondary_audio_type": p.SecondaryAudioType, "secondary_audio_language": p.SecondaryAudioLanguage},
		"recording": {"recording_start_time": video.RecordingStartTime, "recording_end_time": video.RecordingEndTime, "duration": video.Duration},
	}
}
