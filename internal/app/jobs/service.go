package jobs

import (
	"context"
	"errors"
	"time"

	jobconsumer "github.com/dujiao-next/internal/app/jobs/consumer"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/logger"
	settingsintegration "github.com/dujiao-next/internal/modules/settings/schema/integration"
	"github.com/dujiao-next/internal/queue"

	"github.com/hibiken/asynq"
)

// Service 异步队列服务
type Service struct {
	name      string
	server    *asynq.Server
	mux       *asynq.ServeMux
	scheduler *asynq.Scheduler
	consumer  *jobconsumer.Consumer
}

// NewService 创建异步队列服务
func NewService(cfg *config.QueueConfig, consumer *jobconsumer.Consumer) (*Service, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, errors.New("queue disabled")
	}
	if consumer == nil {
		return nil, errors.New("consumer is nil")
	}
	opt, serverCfg := queue.BuildServerConfig(cfg)
	server := asynq.NewServer(opt, serverCfg)
	mux := asynq.NewServeMux()
	consumer.Register(mux)

	scheduler := asynq.NewScheduler(opt, nil)
	registerPeriodicTasks(scheduler, consumer, cfg)

	return &Service{
		name:      "worker",
		server:    server,
		mux:       mux,
		scheduler: scheduler,
		consumer:  consumer,
	}, nil
}

// registerPeriodicTasks 注册所有周期性任务
func registerPeriodicTasks(scheduler *asynq.Scheduler, consumer *jobconsumer.Consumer, cfg *config.QueueConfig) {
	if scheduler == nil || consumer == nil {
		return
	}
	if kafudaRecoveryConfigured(consumer) {
		task := queue.NewKafudaRecoverChecksTask()
		interval := consumer.KafudaGPT.PollInterval(consumer.Config.KafudaPollInterval())
		entryID, err := scheduler.Register("@every "+interval.String(), task, asynq.Queue(queue.DefaultQueue), asynq.Unique(time.Minute))
		if err != nil {
			logger.Warnw("scheduler_register_kafuda_recover_checks_failed", "error", err)
		} else {
			logger.Infow("scheduler_register_kafuda_recover_checks_ok", "entry_id", entryID)
		}
	}
	if kafudaCatalogRefreshConfigured(consumer) {
		interval, fixed := consumer.KafudaGPT.CatalogRefreshSchedule(consumer.Config.KafudaCatalogRefreshInterval(), consumer.Config.KafudaCatalogFixedRefresh())
		mode := "before_expiry"
		if fixed {
			mode = "fixed_interval"
		}
		scheduleInterval := interval
		if scheduleInterval <= 0 {
			scheduleInterval = time.Minute
			mode = "standby"
		}
		task := queue.NewKafudaRefreshCatalogTask()
		uniqueTTL := min(scheduleInterval, time.Minute)
		entryID, err := scheduler.Register("@every "+scheduleInterval.String(), task, asynq.Queue(queue.DefaultQueue), asynq.Unique(uniqueTTL))
		if err != nil {
			logger.Warnw("scheduler_register_kafuda_refresh_catalog_failed", "error", err, "interval", scheduleInterval.String(), "mode", mode)
		} else {
			logger.Infow("scheduler_register_kafuda_refresh_catalog_ok", "entry_id", entryID, "interval", scheduleInterval.String(), "mode", mode)
		}
	}
	if consumer.Quefa != nil {
		pollInterval := 10 * time.Second
		if consumer.Config != nil {
			pollInterval = consumer.Config.QuefaPollInterval()
		}
		task := queue.NewQuefaPollOrdersTask()
		uniqueTTL := min(pollInterval, time.Minute)
		entryID, err := scheduler.Register("@every "+pollInterval.String(), task, asynq.Queue(queue.DefaultQueue), asynq.Unique(uniqueTTL))
		if err != nil {
			logger.Warnw("scheduler_register_quefa_poll_orders_failed", "error", err, "interval", pollInterval.String())
		} else {
			logger.Infow("scheduler_register_quefa_poll_orders_ok", "entry_id", entryID, "interval", pollInterval.String())
		}
	}
	if consumer.AffiliateService != nil {
		task := queue.NewAffiliateConfirmCommissionsTask()
		entryID, err := scheduler.Register("@every 1m", task, asynq.Queue(queue.DefaultQueue))
		if err != nil {
			logger.Warnw("scheduler_register_affiliate_confirm_failed", "error", err)
		} else {
			logger.Infow("scheduler_register_affiliate_confirm_ok", "entry_id", entryID)
		}
	}
	if consumer.ResellerAccountingLedger != nil {
		task := queue.NewResellerConfirmLedgerTask()
		entryID, err := scheduler.Register("@every 1m", task, asynq.Queue(queue.DefaultQueue))
		if err != nil {
			logger.Warnw("scheduler_register_reseller_confirm_ledger_failed", "error", err)
		} else {
			logger.Infow("scheduler_register_reseller_confirm_ledger_ok", "entry_id", entryID)
		}
	}
	if consumer.ProductMappingService != nil {
		fallbackInterval := "5m"
		if cfg != nil && cfg.UpstreamSyncInterval != "" {
			fallbackInterval = cfg.UpstreamSyncInterval
		}
		// 优先读取后台动态设置；失败则回落到 config.yml 的兜底值
		syncInterval := fallbackInterval
		if consumer.SettingService != nil {
			d, err := consumer.SettingService.GetUpstreamSyncInterval(fallbackInterval)
			if err != nil {
				logger.Warnw("scheduler_load_upstream_sync_interval_failed", "error", err, "fallback", fallbackInterval)
			} else if d > 0 {
				syncInterval = settingsintegration.FormatUpstreamSyncIntervalForScheduler(d)
			}
		}
		task := queue.NewUpstreamSyncStockTask()
		entryID, err := scheduler.Register("@every "+syncInterval, task, asynq.Queue(queue.DefaultQueue))
		if err != nil {
			logger.Warnw("scheduler_register_upstream_sync_stock_failed", "error", err)
		} else {
			logger.Infow("scheduler_register_upstream_sync_stock_ok", "entry_id", entryID, "interval", syncInterval)
		}
	}
	if consumer.NotificationService != nil {
		task, err := queue.NewNotificationInventoryAlertCheckTask()
		if err != nil {
			logger.Warnw("scheduler_register_inventory_alert_check_failed", "error", err)
		} else {
			entryID, registerErr := scheduler.Register("@every 1m", task, asynq.Queue(queue.DefaultQueue))
			if registerErr != nil {
				logger.Warnw("scheduler_register_inventory_alert_check_failed", "error", registerErr)
			} else {
				logger.Infow("scheduler_register_inventory_alert_check_ok", "entry_id", entryID)
			}
		}
	}
	if consumer.ProcurementOrderService != nil {
		task := queue.NewProcurementSyncAcceptedTask()
		entryID, err := scheduler.Register("@every 30m", task, asynq.Queue(queue.DefaultQueue))
		if err != nil {
			logger.Warnw("scheduler_register_procurement_sync_accepted_failed", "error", err)
		} else {
			logger.Infow("scheduler_register_procurement_sync_accepted_ok", "entry_id", entryID)
		}
	}
}

func kafudaRecoveryConfigured(consumer *jobconsumer.Consumer) bool {
	return consumer != nil && consumer.Container != nil && consumer.KafudaGPT != nil && consumer.KafudaGPT.Records != nil
}

func kafudaCatalogRefreshConfigured(consumer *jobconsumer.Consumer) bool {
	if consumer == nil || consumer.Container == nil || consumer.KafudaGPT == nil || consumer.KafudaGPT.Catalog == nil {
		return false
	}
	if consumer.KafudaGPT.Config != nil {
		return true
	}
	interval, _ := consumer.KafudaGPT.CatalogRefreshSchedule(consumer.Config.KafudaCatalogRefreshInterval(), consumer.Config.KafudaCatalogFixedRefresh())
	return consumer.KafudaGPT.GatewayRef() != nil && interval > 0
}

func kafudaCatalogRefreshActive(consumer *jobconsumer.Consumer) bool {
	if consumer == nil || consumer.Container == nil || consumer.KafudaGPT == nil || consumer.KafudaGPT.Catalog == nil {
		return false
	}
	interval, _ := consumer.KafudaGPT.CatalogRefreshSchedule(consumer.Config.KafudaCatalogRefreshInterval(), consumer.Config.KafudaCatalogFixedRefresh())
	return consumer.KafudaGPT.GatewayRef() != nil && interval > 0
}

// Name 服务名称
func (s *Service) Name() string {
	if s == nil || s.name == "" {
		return "worker"
	}
	return s.name
}

// Start 启动服务
func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.server == nil || s.mux == nil {
		return errors.New("worker not initialized")
	}
	s.startScheduler(ctx)
	return s.server.Run(s.mux)
}

func (s *Service) startScheduler(ctx context.Context) {
	if s.scheduler == nil {
		return
	}
	if err := s.scheduler.Start(); err != nil {
		logger.Warnw("scheduler_start_failed", "error", err)
		return
	}
	recoverConfigured := kafudaRecoveryConfigured(s.consumer)
	refreshConfigured := kafudaCatalogRefreshActive(s.consumer)
	if !recoverConfigured && !refreshConfigured {
		return
	}
	// Recover existing work and warm the catalog cache at startup before the first periodic tick.
	enqueueCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if recoverConfigured {
		if err := s.consumer.QueueClient.EnqueueKafudaRecoverChecks(enqueueCtx); err != nil {
			logger.Warnw("scheduler_enqueue_kafuda_recover_checks_failed", "error", err)
		}
	}
	if refreshConfigured {
		if err := s.consumer.QueueClient.EnqueueKafudaRefreshCatalog(enqueueCtx); err != nil {
			logger.Warnw("scheduler_enqueue_kafuda_refresh_catalog_failed", "error", err)
		}
	}
}

// Stop 停止服务
func (s *Service) Stop(ctx context.Context) error {
	if s == nil || s.server == nil {
		return nil
	}
	_ = ctx
	if s.scheduler != nil {
		s.scheduler.Shutdown()
	}
	s.server.Shutdown()
	return nil
}
