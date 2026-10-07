package videostream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// finiteInputGate は指定バイト数を fake が記録した次の Read を止める。
// sleep で順序を推測せず、テストから release するまで EOF も公開しない。
type finiteInputGate struct {
	reader  io.ReadCloser
	limit   int
	read    int
	reached chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
	passed  bool
}

func newFiniteInputGate(limit int) *finiteInputGate {
	return &finiteInputGate{
		limit: limit, reached: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (g *finiteInputGate) Read(buffer []byte) (int, error) {
	if !g.passed && g.read == g.limit {
		close(g.reached)
		select {
		case <-g.release:
			g.passed = true
		case <-g.closed:
			return 0, io.ErrClosedPipe
		}
	}
	if !g.passed && len(buffer) > g.limit-g.read {
		buffer = buffer[:g.limit-g.read]
	}
	n, err := g.reader.Read(buffer)
	g.read += n
	return n, err
}

func (g *finiteInputGate) Close() error {
	g.once.Do(func() { close(g.closed) })
	return g.reader.Close()
}

// gatedInputProcess は production の feeder を同じ io.Pipe に接続する。
// Kill で gate と pipe の両方を解除し、production の cleanup は変更しない。
type gatedInputProcess struct {
	Process
	writer *io.PipeWriter
	gate   *finiteInputGate
}

func (p *gatedInputProcess) Stdin() io.WriteCloser { return p.writer }
func (p *gatedInputProcess) Kill() error {
	err := p.Process.Kill()
	_ = p.gate.Close()
	_ = p.writer.Close()
	return err
}

// TestFakeEncoderWaitsForFiniteInput は固定 oracle 用の有限入力だけを対象にする。
// 実エンコーダーが全入力の EOF まで出力を待つ、という一般契約ではない。
// synctest は gate 中の早期終了を決定的に観測し、残留 goroutine も検出する。
func TestFakeEncoderWaitsForFiniteInput(t *testing.T) {
	_, scenario, synth, info := scenarioForTest(t, "ts_ffmpeg_final")
	wantInput := scenario.Result.TSReadExStdin["0"]
	for _, tc := range []struct {
		name  string
		limit int
	}{
		{"zero", 0}, {"seven_bytes", 7}, {"pat_pmt_376", 376}, {"full_data", wantInput.Length},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := newFiniteInputGate(tc.limit)
				runner := &fakeRunner{encoderOutput: func(int) []byte { return synth }}
				session := sessionFromScenario(t, scenario, info)
				task := newTestTask(session, runner, true)
				var gated *gatedInputProcess
				task.options.Runner = func(spec ProcessSpec) (Process, error) {
					if spec.Kind == ProcessTSReadEx && spec.StdinPipe {
						reader, writer := io.Pipe()
						gate.reader = reader
						spec.StdinPipe = false
						spec.Stdin = gate
						process, err := runner.run(spec)
						gated = &gatedInputProcess{Process: process, writer: writer, gate: gate}
						return gated, err
					}
					if spec.Kind == ProcessEncoder {
						// 入力 prefix が記録された後に初めて偽エンコーダーを起動する。
						<-gate.reached
					}
					return runner.run(spec)
				}
				done := make(chan error, 1)
				go func() { done <- task.Run(context.Background(), scenario.Input.StartSequence) }()
				synctest.Wait()
				defer func() {
					if gated != nil {
						_ = gated.Kill()
					}
					for _, process := range runner.procs {
						_ = process.Kill()
					}
				}()

				select {
				case <-gate.reached:
				default:
					t.Fatal("stdin consumer did not reach the gate")
				}
				prefix := runner.procs[0].receivedData()
				t.Logf("gated stdin: limit=%d length=%d sha256=%s", tc.limit, len(prefix), sha256Hex(prefix))
				if len(prefix) != tc.limit {
					t.Errorf("gated stdin length=%d, want %d", len(prefix), tc.limit)
				}
				if task.IsFinished() {
					t.Error("fake encoder published final output before finite input drained")
				}
				for i, segment := range session.segments {
					if status, _ := segment.snapshot(); status == "Completed" {
						t.Errorf("segment %d completed before finite input drained", i)
					}
				}
				close(gate.release)
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("Run did not finish after releasing finite input")
				}

				data := runner.procs[0].receivedData()
				if len(data) != wantInput.Length || sha256Hex(data) != wantInput.SHA256 {
					t.Errorf("tsreadex[0] stdin: length=%d sha=%s, want length=%d sha=%s", len(data), sha256Hex(data), wantInput.Length, wantInput.SHA256)
				}
				for i, want := range scenario.Result.Segments {
					status, data := session.segments[i].snapshot()
					if status != want.Status || len(data) != *want.Length || sha256Hex(data) != *want.SHA256 {
						t.Errorf("segment %d does not match the frozen oracle", i)
					}
				}
				if task.RetryCount() != scenario.Result.RetryCount || task.IsFinished() != scenario.Result.IsFinished {
					t.Error("task result does not match the frozen oracle")
				}
				keyFrames := session.sortedUniqueKeyFrames()
				if len(keyFrames) != len(scenario.Result.KeyFrames) {
					t.Fatalf("keyframe count=%d, want %d", len(keyFrames), len(scenario.Result.KeyFrames))
				}
				for i, want := range scenario.Result.KeyFrames {
					if keyFrames[i].Offset != want.Offset || keyFrames[i].DTS != want.DTS {
						t.Errorf("keyframe[%d]=%+v, want %+v", i, keyFrames[i], want)
					}
				}
			})
		})
	}
}

// TestFakeEncoderKillUnblocksDrainWait は上流が EOF を出さなくても Kill で戻ることを検証する。
// synctest の終了時には出力待ち・stdin 読み取り・終了監視の全 goroutine が終了している。
func TestFakeEncoderKillUnblocksDrainWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &fakeRunner{encoderOutput: func(int) []byte { return []byte("output") }}
		upstream, err := runner.run(ProcessSpec{Kind: ProcessTSReadEx, StdinPipe: true})
		if err != nil {
			t.Fatal(err)
		}
		defer upstream.Kill()
		encoder, err := runner.run(ProcessSpec{Kind: ProcessEncoder, Stdin: upstream.Stdout()})
		if err != nil {
			t.Fatal(err)
		}
		defer encoder.Kill()
		var output []byte
		readDone := make(chan struct{})
		go func() { output, _ = io.ReadAll(encoder.Stdout()); close(readDone) }()
		synctest.Wait()
		select {
		case <-readDone:
			t.Fatal("output completed before upstream EOF or Kill")
		default:
		}

		// 同時・複数回の Kill と出力側の終了が競合しても二重 close してはならない。
		var kills sync.WaitGroup
		for range 8 {
			kills.Go(func() { _ = encoder.Kill() })
		}
		kills.Wait()
		synctest.Wait()
		select {
		case <-readDone:
		default:
			t.Fatal("Kill did not unblock encoder stdout")
		}
		if err := encoder.Wait(); err != nil || len(output) != 0 {
			t.Fatalf("killed encoder: err=%v output=%q", err, output)
		}
		select {
		case <-upstream.(*fakeProcess).inputDrained:
			t.Fatal("encoder Kill incorrectly drained upstream")
		default:
		}
		_ = upstream.Kill()
		_ = upstream.Kill()
		synctest.Wait()
		select {
		case <-upstream.(*fakeProcess).inputDrained:
		default:
			t.Fatal("upstream Kill did not unblock stdin consumer")
		}
		if got := strings.Join(runner.killOrder, ","); got != "encoder,tsreadex" {
			t.Errorf("kill order=%s, want encoder,tsreadex", got)
		}
	})
}

// TestFakeEncoderOutputCloseUnblocksWriter は下流が途中で読むのを止める経路を検証する。
func TestFakeEncoderOutputCloseUnblocksWriter(t *testing.T) {
	for _, action := range []string{"close", "kill"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner := &fakeRunner{encoderOutput: func(int) []byte { return []byte("unread output") }}
				upstream, err := runner.run(ProcessSpec{Kind: ProcessTSReadEx})
				if err != nil {
					t.Fatal(err)
				}
				defer upstream.Kill()
				encoder, err := runner.run(ProcessSpec{Kind: ProcessEncoder, Stdin: upstream.Stdout()})
				if err != nil {
					t.Fatal(err)
				}
				defer encoder.Kill()
				synctest.Wait()
				select {
				case <-encoder.(*fakeProcess).done:
					t.Fatal("encoder finished before its blocked output write")
				default:
				}
				if action == "close" {
					_ = encoder.Stdout().Close()
					_ = encoder.Stdout().Close()
				} else {
					_ = encoder.Kill()
					_ = encoder.Kill()
				}
				synctest.Wait()
				select {
				case <-encoder.(*fakeProcess).done:
				default:
					t.Fatal("closing output did not stop encoder writer")
				}
				if err := encoder.Wait(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

// finiteInputErrorReader はバイト列と非 EOF エラーを同じ Read で返す。
type finiteInputErrorReader struct{ data []byte }

func (r *finiteInputErrorReader) Read(buffer []byte) (int, error) {
	n := copy(buffer, r.data)
	r.data = r.data[n:]
	return n, io.ErrUnexpectedEOF
}

// TestFakeEncoderFiniteInputTermination は空入力・エラー・pipe close でも待機が解除されることを検証する。
func TestFakeEncoderFiniteInputTermination(t *testing.T) {
	for _, mode := range []string{"no_upstream", "nil_input", "reader_eof", "reader_error", "pipe_close"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				wantOutput := []byte("finite output")
				runner := &fakeRunner{encoderOutput: func(int) []byte { return wantOutput }}
				var upstream Process
				var wantInput []byte
				if mode != "no_upstream" {
					spec := ProcessSpec{Kind: ProcessTSReadEx}
					switch mode {
					case "reader_eof":
						wantInput = []byte("finite input")
						spec.Stdin = bytes.NewReader(wantInput)
					case "reader_error":
						wantInput = []byte("partial input")
						spec.Stdin = &finiteInputErrorReader{data: wantInput}
					case "pipe_close":
						spec.StdinPipe = true
					}
					var err error
					upstream, err = runner.run(spec)
					if err != nil {
						t.Fatal(err)
					}
					defer upstream.Kill()
					if mode == "pipe_close" {
						_ = upstream.Stdin().Close()
						_ = upstream.Stdin().Close()
					}
				}
				encoder, err := runner.run(ProcessSpec{Kind: ProcessEncoder})
				if err != nil {
					t.Fatal(err)
				}
				defer encoder.Kill()
				output, err := io.ReadAll(encoder.Stdout())
				if err != nil || !bytes.Equal(output, wantOutput) {
					t.Fatalf("output=%q err=%v", output, err)
				}
				synctest.Wait()
				if upstream != nil {
					process := upstream.(*fakeProcess)
					select {
					case <-process.inputDrained:
					default:
						t.Fatal("output published before input termination")
					}
					if !bytes.Equal(process.receivedData(), wantInput) {
						t.Errorf("recorded input=%q, want %q", process.receivedData(), wantInput)
					}
				}
			})
		})
	}
}

// TestFakeEncoderRetryWaitsForCurrentInput は前の試行の終了通知を使い回さないことを検証する。
func TestFakeEncoderRetryWaitsForCurrentInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &fakeRunner{encoderOutput: func(attempt int) []byte { return []byte{byte(attempt)} }}
		for attempt := range 2 {
			upstream, err := runner.run(ProcessSpec{Kind: ProcessTSReadEx, StdinPipe: true})
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Kill()
			encoder, err := runner.run(ProcessSpec{Kind: ProcessEncoder, Stdin: upstream.Stdout()})
			if err != nil {
				t.Fatal(err)
			}
			defer encoder.Kill()
			var output []byte
			var readErr error
			readDone := make(chan struct{})
			go func() { output, readErr = io.ReadAll(encoder.Stdout()); close(readDone) }()
			synctest.Wait()
			select {
			case <-readDone:
				t.Fatalf("attempt %d output completed before current input", attempt)
			default:
			}
			if _, err := upstream.Stdin().Write([]byte{byte(attempt)}); err != nil {
				t.Fatal(err)
			}
			_ = upstream.Stdin().Close()
			<-readDone
			if readErr != nil || !bytes.Equal(output, []byte{byte(attempt)}) {
				t.Fatalf("attempt %d output=%v err=%v", attempt, output, readErr)
			}
			_ = encoder.Kill()
			_ = upstream.Kill()
		}
	})
}

// TestFakeBlockingEncoderPreservesCancelContract はキャンセル専用 fake の従来契約を維持する。
// 入力 EOF を待たずに出力し、出力を読み終えても Kill までは EOF/Wait を返さない。
func TestFakeBlockingEncoderPreservesCancelContract(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &fakeRunner{blockProcesses: true, encoderOutput: func(int) []byte { return []byte("streamed") }}
		upstream, err := runner.run(ProcessSpec{Kind: ProcessTSReadEx, StdinPipe: true})
		if err != nil {
			t.Fatal(err)
		}
		defer upstream.Kill()
		encoder, err := runner.run(ProcessSpec{Kind: ProcessEncoder, Stdin: upstream.Stdout()})
		if err != nil {
			t.Fatal(err)
		}
		defer encoder.Kill()
		output := make([]byte, len("streamed"))
		if _, err := io.ReadFull(encoder.Stdout(), output); err != nil || string(output) != "streamed" {
			t.Fatalf("streaming output=%q err=%v", output, err)
		}
		readDone := make(chan error, 1)
		go func() { _, err := io.Copy(io.Discard, encoder.Stdout()); readDone <- err }()
		synctest.Wait()
		select {
		case <-readDone:
			t.Fatal("blocking encoder published EOF before Kill")
		case <-encoder.(*fakeProcess).done:
			t.Fatal("blocking encoder finished before Kill")
		case <-upstream.(*fakeProcess).inputDrained:
			t.Fatal("upstream input drained unexpectedly")
		default:
		}
		_ = encoder.Kill()
		_ = encoder.Kill()
		err = <-readDone
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
		_ = upstream.Kill()
		if err := encoder.Wait(); err != nil {
			t.Fatal(err)
		}
	})
}
