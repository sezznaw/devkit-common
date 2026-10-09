package kafkax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/zlog"
)

// The dead-letter tool. A message whose handler kept failing was parked in
// <topic>.dlq with the error in a header (park). `<binary> --dlq ...` looks at
// those messages and puts them back: it reads each dlq topic of the topics
// this service subscribes from the position its tool group ("<service>-dlq")
// last committed, so a message is pending until it is replayed or dropped.
//
//	--dlq list                      pending messages of every dlq, newest last
//	--dlq show <topic> <offset>     one message in full (headers, payload)
//	--dlq replay <topic>            re-publish every pending message to <topic>, then mark them done
//	--dlq replay <topic> <offset>   re-publish one (not marked: drop or replay-all later)
//	--dlq drop <topic>              mark every pending message done without replaying
//
// A replayed message is the original record (key, value, trace headers) with
// a replayed-from header, so the consumer handles it exactly as it would
// have the first time; the fix must be deployed before replaying.
//
// 死信工具。handler 一直失败的消息被放进 <topic>.dlq（park，错误在 header 里）。
// `<二进制> --dlq ...` 查看并放回：从工具组（"<服务>-dlq"）上次提交的位置读每个死信主题，
// 一条消息在被回放或丢弃之前都算待处理。回放的是原始记录（key、value、trace 头）加一个
// replayed-from 头，消费者像第一次一样处理；先把修复发上去再回放。
func (c *Client) DLQ(ctx context.Context, args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: --dlq list | show <topic> <offset> | replay <topic> [<offset>] | drop <topic>")
	}
	adm := kadm.NewClient(c.prod)
	group := c.service + "-dlq"
	switch args[0] {
	case "list":
		topics := c.dlqTopics()
		if len(topics) == 0 {
			fmt.Fprintln(w, "this service subscribes no topic; nothing can be in a dead-letter queue")
			return nil
		}
		total := 0
		for _, t := range topics {
			recs, err := c.dlqPending(ctx, adm, group, t)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "%s: %d pending\n", t, len(recs))
			for _, r := range recs {
				fmt.Fprintf(w, "  %s\n", summary(r))
			}
			total += len(recs)
		}
		if total == 0 {
			fmt.Fprintln(w, "no pending dead letters")
		}
		return nil
	case "show":
		if len(args) < 3 {
			return errors.New("usage: --dlq show <topic> <offset>")
		}
		off, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return fmt.Errorf("offset %q is not a number", args[2])
		}
		recs, err := c.dlqPending(ctx, adm, group, strings.TrimSuffix(args[1], DLQSuffix)+DLQSuffix)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.Offset == off {
				fmt.Fprintln(w, summary(r))
				for _, h := range r.Headers {
					fmt.Fprintf(w, "  %s: %s\n", h.Key, h.Value)
				}
				fmt.Fprintf(w, "  payload: %s\n", r.Value)
				return nil
			}
		}
		return fmt.Errorf("no pending message at offset %d (list shows the pending ones)", off)
	case "replay", "drop":
		if len(args) < 2 {
			return fmt.Errorf("usage: --dlq %s <topic> [<offset>]", args[0])
		}
		source := strings.TrimSuffix(args[1], DLQSuffix)
		dlq := source + DLQSuffix
		var only *int64
		if len(args) >= 3 {
			off, err := strconv.ParseInt(args[2], 10, 64)
			if err != nil {
				return fmt.Errorf("offset %q is not a number", args[2])
			}
			only = &off
		}
		recs, err := c.dlqPending(ctx, adm, group, dlq)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			fmt.Fprintf(w, "%s: nothing pending\n", dlq)
			return nil
		}
		if args[0] == "replay" {
			n := 0
			for _, r := range recs {
				if only != nil && r.Offset != *only {
					continue
				}
				if err := c.replay(ctx, source, r); err != nil {
					return err
				}
				fmt.Fprintf(w, "replayed to %s: %s\n", source, summary(r))
				n++
			}
			if only != nil {
				if n == 0 {
					return fmt.Errorf("no pending message at offset %d", *only)
				}
				fmt.Fprintln(w, "(one message replayed; the queue position is unchanged: `--dlq drop` or a full replay marks it done)")
				return nil
			}
		}
		if err := c.dlqMarkDone(ctx, adm, group, dlq, recs); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s: %d message(s) marked done\n", dlq, len(recs))
		return nil
	default:
		return fmt.Errorf("unknown --dlq command %q: list | show | replay | drop", args[0])
	}
}

func (c *Client) dlqTopics() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.subs {
		out = append(out, s.topic+DLQSuffix)
	}
	sort.Strings(out)
	return out
}

// dlqPending reads the records of the dlq topic between the tool group's
// committed offsets and the end offsets.
func (c *Client) dlqPending(ctx context.Context, adm *kadm.Client, group, dlq string) ([]*kgo.Record, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ends, err := adm.ListEndOffsets(ctx, dlq)
	if err != nil {
		return nil, fmt.Errorf("end offsets of %s: %w", dlq, err)
	}
	committed, err := adm.FetchOffsets(ctx, group)
	if err != nil {
		if !errors.Is(err, kerr.GroupIDNotFound) { // never committed: everything is pending
			return nil, fmt.Errorf("committed offsets of %s: %w", group, err)
		}
		committed = nil
	}
	starts := map[int32]kgo.Offset{}
	want := map[int32]int64{}
	ends.Each(func(o kadm.ListedOffset) {
		if o.Err != nil {
			return
		}
		from := int64(0)
		if committed != nil {
			if co, ok := committed.Lookup(dlq, o.Partition); ok && co.At >= 0 {
				from = co.At
			}
		}
		if from < o.Offset {
			starts[o.Partition] = kgo.NewOffset().At(from)
			want[o.Partition] = o.Offset
		}
	})
	if len(starts) == 0 {
		return nil, nil
	}
	cl, err := kgo.NewClient(append(c.baseOpts(), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{dlq: starts}))...)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	var out []*kgo.Record
	done := map[int32]bool{}
	for len(done) < len(want) {
		fctx, fcancel := context.WithTimeout(ctx, 3*time.Second)
		fetches := cl.PollFetches(fctx)
		fcancel()
		if ctx.Err() != nil {
			break
		}
		fetches.EachRecord(func(r *kgo.Record) {
			if r.Offset < want[r.Partition] {
				out = append(out, r)
			}
			if r.Offset+1 >= want[r.Partition] {
				done[r.Partition] = true
			}
		})
		if fetches.Empty() {
			break // nothing more arrives: the partitions are read
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.Before(out[j].Timestamp) })
	return out, nil
}

// replay re-publishes a parked record to its source topic.
func (c *Client) replay(ctx context.Context, source string, r *kgo.Record) error {
	var headers []kgo.RecordHeader
	for _, h := range r.Headers {
		switch h.Key {
		case "error", "source-topic", "consumer", "replayed-from":
			continue
		}
		headers = append(headers, h)
	}
	headers = append(headers, kgo.RecordHeader{Key: "replayed-from", Value: []byte(fmt.Sprintf("%s@%d", r.Topic, r.Offset))})
	rec := &kgo.Record{Topic: source, Key: r.Key, Value: r.Value, Headers: headers}
	if err := c.prod.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("replay %s@%d: %w", r.Topic, r.Offset, err)
	}
	zlog.Info("dead letter replayed", zlog.Str("dlq", r.Topic), zlog.Int("offset", r.Offset), zlog.Str("key", string(r.Key)), zlog.Str("to", source))
	return nil
}

// dlqMarkDone commits the tool group past the records.
func (c *Client) dlqMarkDone(ctx context.Context, adm *kadm.Client, group, dlq string, recs []*kgo.Record) error {
	var offs kadm.Offsets
	for _, r := range recs {
		if cur, ok := offs.Lookup(dlq, r.Partition); !ok || r.Offset+1 > cur.At {
			offs.Add(kadm.Offset{Topic: dlq, Partition: r.Partition, At: r.Offset + 1})
		}
	}
	resp, err := adm.CommitOffsets(ctx, group, offs)
	if err != nil {
		return fmt.Errorf("commit %s: %w", group, err)
	}
	return resp.Error()
}

func summary(r *kgo.Record) string {
	var ev Event
	_ = json.Unmarshal(r.Value, &ev)
	h := func(k string) string {
		for _, x := range r.Headers {
			if x.Key == k {
				return string(x.Value)
			}
		}
		return ""
	}
	errText := h("error")
	if len(errText) > 120 {
		errText = errText[:120] + "…"
	}
	return fmt.Sprintf("offset=%d partition=%d time=%s key=%s type=%s id=%s error=%q", r.Offset, r.Partition, r.Timestamp.UTC().Format(time.RFC3339), r.Key, ev.Type, ev.ID, errText)
}
