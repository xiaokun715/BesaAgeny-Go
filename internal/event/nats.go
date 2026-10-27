package event

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// NATSBus 是跨进程事件总线与作业队列。
//
// 事件主题：besa.events.<event.name>
// 作业主题：besa.jobs.<kind>，用 JetStream 持久化 + 队列组消费。
type NATSBus struct {
	NC     *nats.Conn
	JS     jetstream.JetStream
	Prefix string
}

func ConnectNATS(url, prefix string) (*NATSBus, error) {
	nc, err := nats.Connect(url,
		nats.Name("besa-agent"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("连接 NATS: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, err
	}
	if prefix == "" {
		prefix = "besa"
	}
	bus := &NATSBus{NC: nc, JS: js, Prefix: prefix}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      "BESA",
		Subjects:  []string{prefix + ".events.>", prefix + ".jobs.>", prefix + ".results.>"},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Discard:   jetstream.DiscardOld,
		MaxMsgs:   2_000_000,
	}); err != nil {
		nc.Close()
		return nil, fmt.Errorf("创建 NATS stream: %w", err)
	}
	return bus, nil
}

func (b *NATSBus) Close() {
	if b.NC != nil {
		_ = b.NC.Drain()
	}
}

func (b *NATSBus) subjectEvent(name string) string { return b.Prefix + ".events." + name }
func (b *NATSBus) subjectJob(kind string) string   { return b.Prefix + ".jobs." + kind }

// Emit 把事件发布到总线。**失败只记录，不影响主流程。**
func (b *NATSBus) Emit(_ context.Context, ev Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if err := b.NC.Publish(b.subjectEvent(ev.Name), raw); err != nil {
		slog.Debug("事件发布失败", "name", ev.Name, "err", err)
	}
}

// SubscribeEvents 订阅某类事件（name 为空 = 全部）。
func (b *NATSBus) SubscribeEvents(name string, handler func(Event)) (*nats.Subscription, error) {
	subject := b.Prefix + ".events."
	if name == "" {
		subject += ">"
	} else {
		subject += name
	}
	return b.NC.Subscribe(subject, func(msg *nats.Msg) {
		var ev Event
		if err := json.Unmarshal(msg.Data, &ev); err == nil {
			handler(ev)
		}
	})
}

// EnsureJobConsumer 创建持久化拉取消费者。
func (b *NATSBus) EnsureJobConsumer(ctx context.Context, kind, durable string) (jetstream.Consumer, error) {
	stream, err := b.JS.Stream(ctx, "BESA")
	if err != nil {
		return nil, err
	}
	return stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: b.subjectJob(kind),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       10 * time.Minute,
		MaxDeliver:    5,
		MaxAckPending: 64,
	})
}

// PublishJob 投递一个作业。msgID 用作 Nats-Msg-Id，实现发布侧去重。
func (b *NATSBus) PublishJob(ctx context.Context, kind, msgID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = b.JS.Publish(ctx, b.subjectJob(kind), raw, jetstream.WithMsgID(msgID))
	return err
}

// ConsumeJob 消费作业；handler 返回 nil 才 ack，否则 nak（重投）。
func (b *NATSBus) ConsumeJob(ctx context.Context, kind, durable string, handler func(ctx context.Context, payload []byte) error) error {
	consumer, err := b.EnsureJobConsumer(ctx, kind, durable)
	if err != nil {
		return err
	}
	iter, err := consumer.Messages()
	if err != nil {
		return err
	}
	defer iter.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msg, err := iter.Next()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if err := handler(ctx, msg.Data()); err != nil {
			slog.Warn("作业处理失败，稍后重投", "kind", kind, "err", err)
			_ = msg.Nak()
			continue
		}
		_ = msg.Ack()
	}
}

// PublishResult 把作业结果发回控制面。
func (b *NATSBus) PublishResult(ctx context.Context, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = b.JS.Publish(ctx, b.Prefix+".results."+key, raw)
	return err
}
