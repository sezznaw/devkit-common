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
// those messages and runs them again: it reads each dlq topic of the topics
// this service subscribes from the position its tool group ("<service>-dlq")
// last committed, so a message is pending until it is replayed or dropped.
//
//	--dlq list                      pending messages of every dlq, newest last
//	--dlq show <topic> <offset>     one message in full (headers, payload)
//	--dlq replay <topic>            run the handler of <topic> on every pending message of this service, in this process; mark the ones that succeed done
//	--dlq replay <topic> <offset>   run it on one (marked done when it is the first pending one of its partition)
//	--dlq drop <topic>              mark every pending message done without replaying
//
// A replay is the handler of this process executing the original record
// (key, value, trace headers, plus a replayed-from header) as it would have
// the first time, so the fix must be deployed before replaying, and the
// outcome is known when the command returns: a message whose handler still
// fails stays pending (nothing is parked again, nothing is published), and
// the partition's position stops in front of it. Nothing is written back to
// the source topic: a consumer needs no write permission on it, and the
// other consumers of the topic are not made to process the message again.
// Several services park into the same <topic>.dlq; each one replays only
// the messages it parked (the consumer header), and skips the others.
//
// 死信工具。handler 一直失败的消息被放进 <topic>.dlq（park，错误在 header 里）。
// `<二进制> --dlq ...` 查看并重跑：从工具组（"<服务>-dlq"）上次提交的位置读每个死信主题，
// 一条消息在被回放或丢弃之前都算待处理。回放 = 本进程的 handler 直接执行原始记录（key、value、
// trace 头，加一个 replayed-from 头），像第一次一样处理，所以先把修复发上去；命令返回时就知道结果：
// 仍失败的留在原地（不会再进一条死信，也不发布任何消息），该分区的位置停在它前面。不写回源主题：
// 消费方不需要源主题的写权限，主题的其它消费者也不会被迫重处理。几个服务共用一个 <topic>.dlq 时，
// 各自只回放自己 park 的记录（consumer 头），跳过别人的。
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
			mine, others := 0, 0
			for _, r := range recs {
				if c.parkedHere(r) {
					mine++
				} else {
					others++
				}
			}
			switch {
			case others == 0:
				fmt.Fprintf(w, "%s: %d pending\n", t, mine)
			default:
				fmt.Fprintf(w, "%s: %d pending, %d parked by other services (they replay those themselves)\n", t, mine, others)
			}
			for _, r := range recs {
				if c.parkedHere(r) {
					fmt.Fprintf(w, "  %s\n", summary(r))
				} else {
					fmt.Fprintf(w, "  %s (parked by %s)\n", summary(r), consumerOf(r))
				}
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
		h := c.handlerOf(source)
		if h == nil {
			return fmt.Errorf("this service does not subscribe %s; only its consumer can replay or drop its dead letters", source)
		}
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
		if args[0] == "drop" {
			done := map[int32]int64{}
			for _, r := range recs {
				if r.Offset+1 > done[r.Partition] {
					done[r.Partition] = r.Offset + 1
				}
			}
			if err := c.dlqCommit(ctx, adm, group, dlq, done); err != nil {
				return err
			}
			fmt.Fprintf(w, "%s: %d message(s) marked done\n", dlq, len(recs))
			return nil
		}
		if only != nil {
			return c.replayOne(ctx, adm, group, source, recs, *only, h, w)
		}
		return c.replayAll(ctx, adm, group, source, recs, h, w)
	default:
		return fmt.Errorf("unknown --dlq command %q: list | show | replay | drop", args[0])
	}
}

// replayAll runs the handler on every pending message this service parked,
// partition by partition in offset order. The position of a partition is
// committed up to the first message that still fails; the messages of other
// services are skipped (committed past: this tool group is this service's).
func (c *Client) replayAll(ctx context.Context, adm *kadm.Client, group, source string, recs []*kgo.Record, h Handler, w io.Writer) error {
	byPartition := map[int32][]*kgo.Record{}
	for _, r := range recs {
		byPartition[r.Partition] = append(byPartition[r.Partition], r)
	}
	partitions := make([]int32, 0, len(byPartition))
	for p := range byPartition {
		partitions = append(partitions, p)
	}
	sort.Slice(partitions, func(i, j int) bool { return partitions[i] < partitions[j] })
	done := map[int32]int64{} // partition -> offset to commit (one past the last message done)
	replayed, skipped, failed := 0, 0, 0
	var firstErr error
	for _, p := range partitions {
		rs := byPartition[p]
		sort.Slice(rs, func(i, j int) bool { return rs[i].Offset < rs[j].Offset })
		for _, r := range rs {
			if !c.parkedHere(r) {
				skipped++
				done[p] = r.Offset + 1
				continue
			}
			if err := c.replayRecord(ctx, source, r, h); err != nil {
				failed++
				fmt.Fprintf(w, "still failing, left pending: %s: %v\n", summary(r), err)
				if firstErr == nil {
					firstErr = fmt.Errorf("%s@%d: %w", r.Topic, r.Offset, err)
				}
				break // the partition's position stops here
			}
			replayed++
			done[p] = r.Offset + 1
			fmt.Fprintf(w, "replayed (handled in this process): %s\n", summary(r))
		}
	}
	if err := c.dlqCommit(ctx, adm, group, source+DLQSuffix, done); err != nil {
		return err
	}
	marked := replayed + skipped
	fmt.Fprintf(w, "%s: %d replayed, %d parked by other services skipped, %d still failing; %d message(s) marked done\n", source+DLQSuffix, replayed, skipped, failed, marked)
	if firstErr != nil {
		return fmt.Errorf("%d message(s) still fail; the first: %w", failed, firstErr)
	}
	return nil
}

// replayOne runs the handler on one pending message. It is marked done when
// it is the first pending message of its partition (the position can move
// past it); otherwise the position is unchanged and a full replay or a drop
// marks it later.
func (c *Client) replayOne(ctx context.Context, adm *kadm.Client, group, source string, recs []*kgo.Record, offset int64, h Handler, w io.Writer) error {
	var rec *kgo.Record
	for _, r := range recs {
		if r.Offset == offset {
			rec = r
			break
		}
	}
	if rec == nil {
		return fmt.Errorf("no pending message at offset %d (list shows the pending ones)", offset)
	}
	if !c.parkedHere(rec) {
		return fmt.Errorf("the message at offset %d was parked by %s; that service replays it", offset, consumerOf(rec))
	}
	if err := c.replayRecord(ctx, source, rec, h); err != nil {
		return fmt.Errorf("still failing, left pending: %s: %w", summary(rec), err)
	}
	first := true
	for _, r := range recs {
		if r.Partition == rec.Partition && r.Offset < rec.Offset && c.parkedHere(r) {
			first = false
			break
		}
	}
	if !first {
		fmt.Fprintf(w, "replayed (handled in this process): %s\n(an earlier message of the partition is still pending, so the position is unchanged: a full replay or a drop marks this one done)\n", summary(rec))
		return nil
	}
	if err := c.dlqCommit(ctx, adm, group, source+DLQSuffix, map[int32]int64{rec.Partition: rec.Offset + 1}); err != nil {
		return err
	}
	fmt.Fprintf(w, "replayed (handled in this process) and marked done: %s\n", summary(rec))
	return nil
}

// replayRecord rebuilds the original record from the parked one and runs it
// through process: the handler, with the dedupe check and the retries of
// consumption. Nothing is produced.
func (c *Client) replayRecord(ctx context.Context, source string, r *kgo.Record, h Handler) error {
	var headers []kgo.RecordHeader
	for _, hd := range r.Headers {
		switch hd.Key {
		case "error", "source-topic", "consumer", "replayed-from":
			continue
		}
		headers = append(headers, hd)
	}
	headers = append(headers, kgo.RecordHeader{Key: "replayed-from", Value: []byte(fmt.Sprintf("%s@%d", r.Topic, r.Offset))})
	rec := &kgo.Record{Topic: source, Key: r.Key, Value: r.Value, Headers: headers, Partition: r.Partition, Offset: r.Offset, Timestamp: r.Timestamp}
	if err := c.process(ctx, rec, h); err != nil {
		return err
	}
	zlog.Info("dead letter replayed", zlog.Str("dlq", r.Topic), zlog.Int("offset", r.Offset), zlog.Str("key", string(r.Key)), zlog.Str("topic", source))
	return nil
}

// handlerOf is the handler this service subscribed for topic, nil without one.
func (c *Client) handlerOf(topic string) Handler {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.subs {
		if s.topic == topic {
			return s.handler
		}
	}
	return nil
}

// consumerOf is the service that parked the record (the consumer header);
// "" on a record parked before the header existed.
func consumerOf(r *kgo.Record) string {
	for _, h := range r.Headers {
		if h.Key == "consumer" {
			return string(h.Value)
		}
	}
	return ""
}

// parkedHere says whether this service parked the record: its consumer
// header names it, or there is none (an old record).
func (c *Client) parkedHere(r *kgo.Record) bool {
	who := consumerOf(r)
	return who == "" || who == c.service
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
// committed offsets and the end offsets, oldest first.
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

// dlqCommit moves the tool group of dlq to the given offset of each partition.
func (c *Client) dlqCommit(ctx context.Context, adm *kadm.Client, group, dlq string, at map[int32]int64) error {
	if len(at) == 0 {
		return nil
	}
	var offs kadm.Offsets
	for p, off := range at {
		offs.Add(kadm.Offset{Topic: dlq, Partition: p, At: off})
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
