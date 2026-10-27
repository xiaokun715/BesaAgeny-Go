// Package composition 是唯一允许 import 全部业务模块的地方。
//
// 装配顺序有约束：配置 → 注册表（启动期校验）→ 网关 → 工具 → 记忆 → 上下文 → agent → 策略。
// 配置错误必须在进程启动时暴露，而不是等第一次线上调用。
package composition

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/besa-ai/besa-agent/internal/event"
	"github.com/besa-ai/besa-agent/internal/foundation/clock"
	"github.com/besa-ai/besa-agent/internal/foundation/config"
	"github.com/besa-ai/besa-agent/internal/foundation/logx"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
	redisstore "github.com/besa-ai/besa-agent/internal/store/redis"
)

type Options struct {
	Env          string
	ConfigDir    string
	EnvVars      map[string]string
	Clock        clock.Clock
	Events       event.Emitter
	RequireDB    bool
	RequireRedis bool
	RequireNATS  bool
	// NoMigrate 关闭自动迁移（生产建议由独立的 migrate 步骤执行）。
	NoMigrate bool
}

type Base struct {
	Config     *config.Config
	Logger     *slog.Logger
	Clock      clock.Clock
	DB         *postgres.DB
	Redis      *redisstore.Client
	NATS       *event.NATSBus
	Events     event.Emitter
	InMemory   *event.InMemory
	Persistent *event.Persistent
	EventRepo  *postgres.EventRepo

	cancel  context.CancelFunc
	closers []func() error
}

func Open(ctx context.Context, opts Options) (*Base, error) {
	cfg, err := config.Load(opts.Env, opts.ConfigDir, opts.EnvVars)
	if err != nil {
		return nil, err
	}
	level := cfg.String("logging.level", "info")
	format := cfg.String("logging.format", "json")
	logger := logx.New(level, format)
	slog.SetDefault(logger)

	clk := opts.Clock
	if clk == nil {
		clk = clock.System{}
	}
	base := &Base{Config: cfg, Logger: logger, Clock: clk}
	base.InMemory = event.NewInMemory()

	dsn := cfg.String("postgres.dsn", "")
	if dsn != "" && !strings.EqualFold(dsn, "disabled") {
		db, err := postgres.Open(ctx, dsn, postgres.Options{MaxConns: int32(cfg.Int("postgres.pool.max_conns", 10))})
		if err != nil {
			if opts.RequireDB {
				return nil, err
			}
			logger.Warn("Postgres 不可用，降级为进程内存储", "err", err)
		} else {
			base.DB = db
			base.closers = append(base.closers, func() error { db.Close(); return nil })
			if !opts.NoMigrate {
				if err := db.Migrate(ctx); err != nil {
					return nil, fmt.Errorf("迁移失败: %w", err)
				}
			}
			base.EventRepo = &postgres.EventRepo{DB: db}
		}
	} else if opts.RequireDB {
		return nil, fmt.Errorf("该角色需要 Postgres：postgres.dsn 未配置")
	}

	redisURL := cfg.String("redis.url", "")
	if redisURL != "" && !strings.EqualFold(redisURL, "disabled") {
		rc, err := redisstore.Open(ctx, redisURL, cfg.String("redis.prefix", "besa_agent:"))
		if err != nil {
			if opts.RequireRedis {
				return nil, err
			}
			logger.Warn("Redis 不可用：幂等退到数据库慢路径，限流退到进程内", "err", err)
		} else {
			base.Redis = rc
			base.closers = append(base.closers, rc.Close)
		}
	} else if opts.RequireRedis {
		return nil, fmt.Errorf("该角色需要 Redis：redis.url 未配置")
	}

	natsURL := cfg.String("nats.url", "")
	if natsURL != "" && !strings.EqualFold(natsURL, "disabled") {
		bus, err := event.ConnectNATS(natsURL, cfg.String("nats.prefix", "besa"))
		if err != nil {
			if opts.RequireNATS {
				return nil, err
			}
			logger.Warn("NATS 不可用：仅单机模式可用", "err", err)
		} else {
			base.NATS = bus
			base.closers = append(base.closers, func() error { bus.Close(); return nil })
		}
	} else if opts.RequireNATS {
		return nil, fmt.Errorf("该角色需要 NATS：nats.url 未配置")
	}

	if opts.Events != nil {
		base.Events = opts.Events
		return base, nil
	}
	if base.DB != nil {
		runCtx, cancel := context.WithCancel(context.Background())
		base.cancel = cancel
		base.Persistent = event.NewPersistent(base.EventRepo, 8192)
		base.Persistent.Start(runCtx)
	}
	// 单一发布路径：有 DB 时事件先进 outbox，由 relay 发布到 NATS；
	// 没有 DB 时才直接发 NATS。两条路同时走会产生重复事件。
	emitters := []event.Emitter{base.InMemory}
	if base.Persistent != nil {
		emitters = append(emitters, base.Persistent)
	} else if base.NATS != nil {
		emitters = append(emitters, base.NATS)
	}
	base.Events = event.NewMulti(emitters...)
	return base, nil
}

func (b *Base) Close() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.Persistent != nil {
		b.Persistent.Close(5 * time.Second)
	}
	for i := len(b.closers) - 1; i >= 0; i-- {
		_ = b.closers[i]()
	}
}

func (b *Base) Env() string { return b.Config.Env() }

// IsDev 用于决定是否打印冗余信息。
func (b *Base) IsDev() bool {
	return !strings.EqualFold(b.Env(), "prod") && !strings.EqualFold(b.Env(), "production")
}

func (b *Base) MustEnv(name string) string { return os.Getenv(name) }
