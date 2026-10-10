package batch_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alessandrolattao/powertools-lambda-go/batch"
)

var errBoom = errors.New("boom")

func sqsEvent(bodies ...string) events.SQSEvent {
	var event events.SQSEvent
	for i, body := range bodies {
		event.Records = append(event.Records, events.SQSMessage{
			MessageId: "m" + string(rune('0'+i)),
			Body:      body,
		})
	}
	return event
}

// failOn fails the records whose body is "fail" and panics on "panic".
func failOn(_ context.Context, msg events.SQSMessage) error {
	switch msg.Body {
	case "fail":
		return errBoom
	case "panic":
		panic("record handler bug")
	default:
		return nil
	}
}

func ids(resp events.SQSEventResponse) []string {
	out := make([]string, 0, len(resp.BatchItemFailures))
	for _, f := range resp.BatchItemFailures {
		out = append(out, f.ItemIdentifier)
	}
	return out
}

func TestProcessSQS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		bodies     []string
		opts       []batch.Option
		wantIDs    []string
		wantErrors int
	}{
		{name: "all succeed", bodies: []string{"ok", "ok"}, wantIDs: []string{}},
		{name: "some fail", bodies: []string{"ok", "fail", "ok", "panic"}, wantIDs: []string{"m1", "m3"}},
		{name: "all fail", bodies: []string{"fail", "panic"}, wantIDs: []string{"m0", "m1"}, wantErrors: 2},
		{
			name:    "all fail without raising",
			bodies:  []string{"fail", "fail"},
			opts:    []batch.Option{batch.WithRaiseOnEntireBatchFailure(false)},
			wantIDs: []string{"m0", "m1"},
		},
		{
			name:    "concurrent",
			bodies:  []string{"fail", "ok", "ok", "fail", "ok"},
			opts:    []batch.Option{batch.WithConcurrency(3)},
			wantIDs: []string{"m0", "m3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := batch.ProcessSQS(t.Context(), sqsEvent(tc.bodies...), failOn, tc.opts...)
			assert.Equal(t, tc.wantIDs, ids(resp))
			if tc.wantErrors == 0 {
				require.NoError(t, err)
				return
			}
			pe, ok := errors.AsType[*batch.ProcessingError](err)
			require.True(t, ok, "want a *ProcessingError, got %v", err)
			assert.Len(t, pe.Errors, tc.wantErrors)
			require.ErrorIs(t, err, errBoom)
			assert.Contains(t, err.Error(), "All records failed processing. 2 individual errors")
		})
	}
}

func TestResponseWireFormat(t *testing.T) {
	t.Parallel()
	ok, err := batch.ProcessSQS(t.Context(), sqsEvent("ok"), failOn)
	require.NoError(t, err)
	raw, err := json.Marshal(ok)
	require.NoError(t, err)
	assert.JSONEq(t, `{"batchItemFailures":[]}`, string(raw))
	assert.NotNil(t, ok.BatchItemFailures, "a nil list would marshal to null with encoding/json")

	partial, err := batch.ProcessSQS(t.Context(), sqsEvent("ok", "fail"), failOn)
	require.NoError(t, err)
	raw, err = json.Marshal(partial)
	require.NoError(t, err)
	assert.JSONEq(t, `{"batchItemFailures":[{"itemIdentifier":"m1"}]}`, string(raw))
}

func TestPanicBecomesFailure(t *testing.T) {
	t.Parallel()
	var got error
	_, err := batch.ProcessSQS(t.Context(), sqsEvent("ok", "panic"), failOn,
		batch.WithOnFailure(func(_ context.Context, _ events.SQSMessage, err error) { got = err }))
	require.NoError(t, err)
	pe, ok := errors.AsType[*batch.PanicError](got)
	require.True(t, ok)
	assert.Equal(t, "record handler bug", pe.Value)
	assert.Contains(t, string(pe.Stack), "batch_test.failOn")
	assert.Contains(t, pe.Error(), "record handler panicked: record handler bug")
	require.NoError(t, pe.Unwrap())

	errPanic := &batch.PanicError{Value: errBoom}
	require.ErrorIs(t, errPanic, errBoom)
}

func TestEmptyBatch(t *testing.T) {
	t.Parallel()
	_, err := batch.ProcessSQS(t.Context(), events.SQSEvent{}, failOn)
	require.ErrorIs(t, err, batch.ErrUnexpectedBatchType)
	_, err = batch.ProcessKafka(t.Context(), events.KafkaEvent{Records: map[string][]events.KafkaRecord{"t-0": nil}},
		func(context.Context, events.KafkaRecord) error { return nil })
	require.ErrorIs(t, err, batch.ErrUnexpectedBatchType)
}

func fifoEvent(groups ...string) events.SQSEvent {
	var event events.SQSEvent
	for i, group := range groups {
		msg := events.SQSMessage{MessageId: "m" + string(rune('0'+i)), Body: "ok"}
		if group != "" {
			msg.Attributes = map[string]string{"MessageGroupId": group}
		}
		event.Records = append(event.Records, msg)
	}
	return event
}

func TestProcessSQSFIFO(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		groups   []string
		fail     string // message id whose handler fails
		opts     []batch.Option
		wantIDs  []string
		wantRuns []string
		wantErr  error // error of the records that never ran
	}{
		{
			name:     "stops at the first failure",
			groups:   []string{"a", "b", "a", "b"},
			fail:     "m1",
			wantIDs:  []string{"m1", "m2", "m3"},
			wantRuns: []string{"m0", "m1"},
			wantErr:  batch.ErrFIFOShortCircuit,
		},
		{
			name:     "concurrency is ignored",
			groups:   []string{"a", "a", "a"},
			fail:     "m0",
			opts:     []batch.Option{batch.WithConcurrency(8), batch.WithRaiseOnEntireBatchFailure(false)},
			wantIDs:  []string{"m0", "m1", "m2"},
			wantRuns: []string{"m0"},
			wantErr:  batch.ErrFIFOShortCircuit,
		},
		{
			name:     "skips the failed group only",
			groups:   []string{"a", "b", "a", "b", "c"},
			fail:     "m1",
			opts:     []batch.Option{batch.WithSkipGroupOnError()},
			wantIDs:  []string{"m1", "m3"},
			wantRuns: []string{"m0", "m1", "m2", "m4"},
			wantErr:  batch.ErrFIFOGroupShortCircuit,
		},
		{
			name:     "a message without group never fails the others",
			groups:   []string{"", "", "a"},
			fail:     "m0",
			opts:     []batch.Option{batch.WithSkipGroupOnError()},
			wantIDs:  []string{"m0"},
			wantRuns: []string{"m0", "m1", "m2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var runs []string
			skipped := map[string]error{}
			handler := func(_ context.Context, msg events.SQSMessage) error {
				runs = append(runs, msg.MessageId)
				if msg.MessageId == tc.fail {
					return errBoom
				}
				return nil
			}
			opts := append([]batch.Option{
				batch.WithOnFailure(func(_ context.Context, msg events.SQSMessage, err error) {
					if !errors.Is(err, errBoom) {
						skipped[msg.MessageId] = err
					}
				}),
			}, tc.opts...)
			resp, err := batch.ProcessSQSFIFO(t.Context(), fifoEvent(tc.groups...), handler, opts...)
			require.NoError(t, err)
			assert.Equal(t, tc.wantIDs, ids(resp))
			assert.Equal(t, tc.wantRuns, runs)
			for id, err := range skipped {
				require.ErrorIs(t, err, tc.wantErr, id)
			}
		})
	}
}

func TestProcessSQSFIFOFirstRecordFails(t *testing.T) {
	t.Parallel()
	resp, err := batch.ProcessSQSFIFO(t.Context(), fifoEvent("a", "a"), func(context.Context, events.SQSMessage) error {
		return errBoom
	})
	require.ErrorIs(t, err, batch.ErrFIFOShortCircuit)
	require.ErrorIs(t, err, errBoom)
	assert.Equal(t, []string{"m0", "m1"}, ids(resp))
}

func TestProcessKinesis(t *testing.T) {
	t.Parallel()
	event := events.KinesisEvent{Records: []events.KinesisEventRecord{
		{Kinesis: events.KinesisRecord{SequenceNumber: "100", Data: []byte("ok")}},
		{Kinesis: events.KinesisRecord{SequenceNumber: "200", Data: []byte("fail")}},
	}}
	resp, err := batch.ProcessKinesis(t.Context(), event, func(_ context.Context, rec events.KinesisEventRecord) error {
		if string(rec.Kinesis.Data) == "fail" {
			return errBoom
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []events.KinesisBatchItemFailure{{ItemIdentifier: "200"}}, resp.BatchItemFailures)
}

func TestProcessDynamoDB(t *testing.T) {
	t.Parallel()
	event := events.DynamoDBEvent{Records: []events.DynamoDBEventRecord{
		{EventName: "INSERT", Change: events.DynamoDBStreamRecord{SequenceNumber: "1"}},
		{EventName: "REMOVE", Change: events.DynamoDBStreamRecord{SequenceNumber: "2"}},
	}}
	resp, err := batch.ProcessDynamoDB(t.Context(), event, func(_ context.Context, rec events.DynamoDBEventRecord) error {
		if rec.EventName == "INSERT" {
			return errBoom
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []events.DynamoDBBatchItemFailure{{ItemIdentifier: "1"}}, resp.BatchItemFailures)
}

func TestProcessKafka(t *testing.T) {
	t.Parallel()
	event := events.KafkaEvent{Records: map[string][]events.KafkaRecord{
		"orders-10": {{Topic: "orders", Partition: 10, Offset: 1}},
		"orders-9":  {{Topic: "orders", Partition: 9, Offset: 5}, {Topic: "orders", Partition: 9, Offset: 6}},
		"audit-0":   {{Topic: "audit", Partition: 0, Offset: 15}},
	}}
	var order []int64
	resp, err := batch.ProcessKafka(t.Context(), event, func(_ context.Context, rec events.KafkaRecord) error {
		order = append(order, rec.Offset)
		if rec.Offset == 15 || rec.Offset == 6 {
			return errBoom
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{15, 5, 6, 1}, order, "topic by topic, partitions in numeric order")

	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.JSONEq(t, `{"batchItemFailures":[
		{"itemIdentifier":{"partition":"audit-0","offset":15}},
		{"itemIdentifier":{"partition":"orders-9","offset":6}}
	]}`, string(raw))
}

func TestCancellation(t *testing.T) {
	t.Parallel()
	for _, concurrency := range []int{1, 2} {
		t.Run(fmt.Sprint("concurrency ", concurrency), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancelCause(t.Context())
			errStop := errors.New("time is up")
			var ran atomic.Int32
			handler := func(ctx context.Context, msg events.SQSMessage) error {
				ran.Add(1)
				switch msg.MessageId {
				case "m0":
					if concurrency > 1 {
						// Hold the other slot until the end, so that nothing
						// else starts.
						<-ctx.Done()
					}
				case "m1":
					cancel(errStop)
				}
				return nil
			}
			var skipped []error
			resp, err := batch.ProcessSQS(ctx, sqsEvent("a", "b", "c", "d", "e", "f"), handler,
				batch.WithConcurrency(concurrency),
				batch.WithOnFailure(func(_ context.Context, _ events.SQSMessage, err error) {
					skipped = append(skipped, err)
				}))
			require.NoError(t, err)
			assert.Equal(t, []string{"m2", "m3", "m4", "m5"}, ids(resp))
			assert.Equal(t, int32(2), ran.Load())
			for _, err := range skipped {
				require.ErrorIs(t, err, batch.ErrNotProcessed)
				require.ErrorIs(t, err, errStop)
			}
		})
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, opts := range [][]batch.Option{nil, {batch.WithConcurrency(4)}} {
		resp, err := batch.ProcessSQS(ctx, sqsEvent("a", "b"), func(context.Context, events.SQSMessage) error {
			t.Error("no record should run")
			return nil
		}, opts...)
		require.ErrorIs(t, err, batch.ErrNotProcessed)
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, []string{"m0", "m1"}, ids(resp))
	}
}

func TestConcurrencyBound(t *testing.T) {
	t.Parallel()
	const limit = 3
	var inFlight, peak atomic.Int32
	handler := func(context.Context, events.SQSMessage) error {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil
	}
	bodies := make([]string, 20)
	_, err := batch.ProcessSQS(t.Context(), sqsEvent(bodies...), handler, batch.WithConcurrency(limit))
	require.NoError(t, err)
	assert.LessOrEqual(t, peak.Load(), int32(limit))
	assert.Greater(t, peak.Load(), int32(1), "records ran concurrently")
}

func TestHooks(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var trail []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		trail = append(trail, s)
	}
	handler := func(_ context.Context, msg events.SQSMessage) error {
		note("handle " + msg.MessageId)
		if msg.MessageId == "m0" {
			return errBoom
		}
		return nil
	}
	_, err := batch.ProcessSQSFIFO(t.Context(), fifoEvent("a", "a"), handler,
		batch.WithRaiseOnEntireBatchFailure(false),
		batch.WithBeforeRecord(func(_ context.Context, msg events.SQSMessage) { note("before " + msg.MessageId) }),
		batch.WithAfterRecord(func(_ context.Context, msg events.SQSMessage, err error) {
			note("after " + msg.MessageId + " " + errText(err))
		}),
		batch.WithOnFailure(func(_ context.Context, msg events.SQSMessage, err error) {
			note("failure " + msg.MessageId + " " + errText(err))
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"before m0", "handle m0", "failure m0 boom", "after m0 boom",
		"failure m1 a previous record failed processing", "after m1 a previous record failed processing",
	}, trail)
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func TestHookRecordTypeMismatch(t *testing.T) {
	t.Parallel()
	ran := false
	_, err := batch.ProcessSQS(t.Context(), sqsEvent("ok"), func(context.Context, events.SQSMessage) error {
		ran = true
		return nil
	}, batch.WithOnFailure(func(context.Context, events.KinesisEventRecord, error) {}))
	require.ErrorIs(t, err, batch.ErrHookRecordType)
	assert.Contains(t, err.Error(), "events.KinesisEventRecord")
	assert.False(t, ran)
}

func TestWithLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	_, err := batch.ProcessSQSFIFO(t.Context(), fifoEvent("a", "a"), failOn,
		batch.WithLogger(logger), batch.WithRaiseOnEntireBatchFailure(false))
	require.NoError(t, err)
	_, err = batch.ProcessSQS(t.Context(), sqsEvent("fail", "ok"), failOn, batch.WithLogger(logger))
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "only the failure of a handler is logged: %s", buf.String())
	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &record))
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "Record processing exception; skipping this record", record["msg"])
	assert.Equal(t, "boom", record["err"])
}

func TestHandler(t *testing.T) {
	t.Parallel()
	h := batch.Handler(batch.ProcessSQS, failOn, batch.WithRaiseOnEntireBatchFailure(false))
	resp, err := h(t.Context(), sqsEvent("fail"))
	require.NoError(t, err)
	assert.Equal(t, []string{"m0"}, ids(resp))

	kafka := batch.Handler(batch.ProcessKafka, func(context.Context, events.KafkaRecord) error { return errBoom })
	_, err = kafka(t.Context(), events.KafkaEvent{Records: map[string][]events.KafkaRecord{"t-0": {{Topic: "t"}}}})
	require.ErrorIs(t, err, errBoom)
}
