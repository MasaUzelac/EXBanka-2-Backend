// bank-service entrypoint.
//
// bank-service entrypoint.
//
// Starts two servers concurrently:
//   - gRPC server          on 0.0.0.0:50051  (standard net/grpc)
//   - gRPC-Gateway HTTP    on 0.0.0.0:8080   (grpc-gateway/v2 runtime.ServeMux)
//
// The HTTP gateway is a reverse-proxy that translates REST calls into gRPC
// calls against the local gRPC server at localhost:50051.
//
// All configuration is loaded from environment variables via internal/config.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "banka-backend/proto/banka"
	pbactuary "banka-backend/proto/actuary"
	auth "banka-backend/shared/auth"
	"banka-backend/services/bank-service/internal/config"
	"banka-backend/services/bank-service/internal/domain"
	"banka-backend/services/bank-service/internal/handler"
	"banka-backend/services/bank-service/internal/repository"
	"banka-backend/services/bank-service/internal/service"
	"banka-backend/services/bank-service/internal/trading"
	tradingworker "banka-backend/services/bank-service/internal/trading/worker"
	"banka-backend/services/bank-service/internal/transport"
	"banka-backend/services/bank-service/internal/worker"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// grpcLocalHost is the hostname the gRPC-Gateway uses to dial back to the
// local gRPC server. Port is extracted from cfg.GRPCAddr at runtime so the
// two always stay in sync regardless of the GRPC_ADDR env var.
const grpcLocalHost = "localhost"

func main() {
	// ── 1. Config ────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("[main] config error: %v", err)
	}

	// ── 2. Database (GORM + PostgreSQL) ──────────────────────────────────────
	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{})
	if err != nil {
		log.Fatalf("[db] open: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("[db] get underlying sql.DB: %v", err)
	}
	defer sqlDB.Close()

	if err := sqlDB.Ping(); err != nil {
		log.Fatalf("[db] ping: %v", err)
	}
	log.Println("[db] connected to PostgreSQL")

	// ── 3. Wire-up slojeva ───────────────────────────────────────────────────
	currencyRepo := repository.NewCurrencyRepository(db)
	currencyService := service.NewCurrencyService(currencyRepo)

	delatnostRepo := repository.NewDelatnostRepository(db)
	delatnostService := service.NewDelatnostService(delatnostRepo)

	accountRepo := repository.NewAccountRepository(db)
	accountService := service.NewAccountService(accountRepo, currencyRepo)

	recipientRepo := repository.NewPaymentRecipientRepository(db)
	paymentRepo := repository.NewPaymentRepository(db)
	paymentService := service.NewPaymentService(recipientRepo, paymentRepo)

	kreditRepo := repository.NewKreditRepository(db)
	kreditService := service.NewKreditService(kreditRepo)

	karticaRepo := repository.NewKarticaRepository(db)
	berzaRepo := repository.NewBerzaRepository(db)

	// ── Redis store za OTP state (Flow 2) ────────────────────────────────────
	var redisStore domain.CardRequestStore
	var marketModeStore domain.MarketModeStore
	if cfg.RedisURL != "" {
		rs, err := transport.NewRedisCardRequestStore(cfg.RedisURL)
		if err != nil {
			log.Fatalf("[main] Redis konekcija: %v", err)
		}
		redisStore = rs

		ms, err := transport.NewRedisMarketModeStore(cfg.RedisURL)
		if err != nil {
			log.Fatalf("[main] Redis market mode store: %v", err)
		}
		marketModeStore = ms
	} else {
		log.Printf("[main] REDIS_URL nije postavljen — /api/cards/request neće biti funkcionalan")
		redisStore = &transport.NoOpCardRequestStore{}
		marketModeStore = &transport.NoOpMarketModeStore{}
	}

	// ── Notification-service gRPC klijent (sinhronizovano slanje OTP emaila) ─
	var notifClient domain.NotificationSender
	if cfg.NotificationServiceAddr != "" {
		nc, err := transport.NewNotificationServiceClient(cfg.NotificationServiceAddr)
		if err != nil {
			log.Fatalf("[main] notification-service gRPC klijent: %v", err)
		}
		defer nc.Close()
		notifClient = nc
		log.Printf("[main] notification-service gRPC klijent konfigurisan na %s", cfg.NotificationServiceAddr)
	} else {
		log.Printf("[main] NOTIFICATION_SERVICE_ADDR nije postavljen — OTP emailovi neće biti slani")
		notifClient = &transport.NoOpNotificationSender{}
	}

	karticaService := service.NewKarticaService(karticaRepo, cfg.CVVPepper, redisStore, notifClient)
	berzaService := service.NewBerzaService(berzaRepo, marketModeStore)

	listingRepo := repository.NewListingRepository(db)
	listingService := service.NewListingService(listingRepo)

	// ── InstallmentWorker (cron job za automatsku naplatu rata) ───────────────
	var notifPublisher worker.NotificationPublisher
	if cfg.RabbitMQURL != "" {
		log.Printf("[main] RabbitMQ konfigurisan — kreditne notifikacije će biti slane")
		notifPublisher = worker.NewAMQPKreditPublisher(cfg.RabbitMQURL)
	} else {
		log.Printf("[main] RABBITMQ_URL nije postavljen — kreditne notifikacije se samo loguju")
		notifPublisher = &worker.NoOpNotificationPublisher{}
	}

	installmentWorker := worker.NewInstallmentWorker(
		kreditRepo,
		notifPublisher,
		time.Duration(cfg.WorkerIntervalHours)*time.Hour,
		time.Duration(cfg.RetryAfterHours)*time.Hour,
		cfg.LatePaymentPenalty,
	)

	// ── User-service gRPC klijent (za validaciju klijenta pri kreiranju računa) ─
	userClient, err := transport.NewUserServiceClient(cfg.UserServiceAddr)
	if err != nil {
		log.Fatalf("[main] user-service gRPC client: %v", err)
	}
	defer userClient.Close()
	log.Printf("[main] user-service gRPC klijent konfigurisan na %s", cfg.UserServiceAddr)

	// ── Account email publisher ───────────────────────────────────────────────
	var accountPublisher worker.AccountEmailPublisher
	if cfg.RabbitMQURL != "" {
		accountPublisher = worker.NewAMQPAccountPublisher(cfg.RabbitMQURL)
	} else {
		accountPublisher = &worker.NoOpAccountEmailPublisher{}
	}

	actuaryRepo := repository.NewActuaryRepository(sqlDB)
	actuaryService := service.NewActuaryService(actuaryRepo)
	actuaryHandler := handler.NewActuaryHandler(actuaryService)
	internalActuaryHandler := handler.NewInternalActuaryHandler(actuaryService, cfg.JWTAccessSecret)

	exchangeProvider := repository.NewExchangeRateProvider(cfg.ExchangeRateAPIKey, cfg.ExchangeRateAPIBaseURL)
	exchangeTransferRepo := repository.NewExchangeTransferRepository(db)
	exchangeService := service.NewExchangeService(exchangeProvider, exchangeTransferRepo, cfg.ExchangeSpreadRate, cfg.ExchangeProvizijaRate)

	orderRepo := repository.NewOrderRepository(db)
	marginChecker := repository.NewMarginChecker(db)
	fundsManager := repository.NewFundsManager(db, exchangeService)
	tradingService := trading.NewTradingService(orderRepo, listingService, actuaryRepo, marginChecker, fundsManager)

	// ── Trading engine (async order execution) ────────────────────────────────
	marketDataProvider := tradingworker.NewListingMarketDataProvider(listingRepo)
	tradingEngine := tradingworker.NewEngine(orderRepo, marketDataProvider, fundsManager, 0)

	bankHandler := handler.NewBankHandler(currencyService, delatnostService, accountService, paymentService, kreditService, karticaService, berzaService, listingService, exchangeService, tradingService, userClient, accountPublisher)

	receiptHandler := handler.NewPaymentReceiptHandler(paymentService, cfg.JWTAccessSecret)
	marketModeHTTPHandler := handler.NewMarketModeHTTPHandler(marketModeStore, cfg.JWTAccessSecret)
	exchangeTransferHandler := handler.NewExchangeTransferHandler(paymentService, cfg.JWTAccessSecret)
	exchangeRateHandler := handler.NewExchangeRateHandler(exchangeService, cfg.JWTAccessSecret)
	karticaRequestHandler := handler.NewKarticaRequestHandler(karticaService, userClient, cfg.JWTAccessSecret, accountPublisher)
	klientKarticeHandler := handler.NewKlientKarticeHandler(karticaService, cfg.JWTAccessSecret)

	portfolioHandler := handler.NewPortfolioHandler(db, listingService, cfg.JWTAccessSecret)
	taxHandler := handler.NewTaxHandler(db, exchangeService, cfg.JWTAccessSecret)
	myOrdersHandler := handler.NewMyOrdersHandler(tradingService, cfg.JWTAccessSecret)
	fundHandler := handler.NewFundHandler(db, exchangeService, cfg.JWTAccessSecret)

	// ── 4. Auth interceptor ──────────────────────────────────────────────────
	// Sve rute zahtevaju validan JWT access token osim gRPC health check-a.
	authInterceptor := auth.NewAuthInterceptor(cfg.JWTAccessSecret, []string{
		"/grpc.health.v1.Health/Check",
	})

	// ── 5. gRPC server ───────────────────────────────────────────────────────
	grpcSrv := transport.NewGRPCServer(cfg.GRPCAddr, authInterceptor.Unary())
	pb.RegisterBankaServiceServer(grpcSrv.Server(), bankHandler)
	pbactuary.RegisterActuaryServiceServer(grpcSrv.Server(), actuaryHandler)

	// ── 6. gRPC-Gateway: dial the local gRPC server ──────────────────────────
	// Derive the gateway target from cfg.GRPCAddr so they always stay in sync.
	// cfg.GRPCAddr is e.g. "0.0.0.0:50051"; we replace the bind host with
	// localhost because gateway and gRPC server share the same process.
	_, grpcPort, err := net.SplitHostPort(cfg.GRPCAddr)
	if err != nil {
		log.Fatalf("[gateway] invalid GRPC_ADDR %q: %v", cfg.GRPCAddr, err)
	}
	grpcLocalTarget := net.JoinHostPort(grpcLocalHost, grpcPort)
	log.Printf("[gateway] gRPC local target: %s", grpcLocalTarget)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	//nolint:staticcheck // grpc.DialContext is deprecated upstream; tracked for
	// migration to grpc.NewClient in a follow-up task.
	conn, err := grpc.DialContext(
		ctx,
		grpcLocalTarget,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		log.Fatalf("[gateway] dial gRPC backend: %v", err)
	}
	defer conn.Close()

	gwMux := runtime.NewServeMux()
	if err := pb.RegisterBankaServiceHandlerClient(
		ctx,
		gwMux,
		pb.NewBankaServiceClient(conn),
	); err != nil {
		log.Fatalf("[gateway] register handler client: %v", err)
	}
	if err := pbactuary.RegisterActuaryServiceHandlerClient(
		ctx,
		gwMux,
		pbactuary.NewActuaryServiceClient(conn),
	); err != nil {
		log.Fatalf("[gateway] register actuary handler client: %v", err)
	}

	// Kombinovani HTTP mux: gRPC-Gateway + direktni HTTP handleri.
	httpMux := http.NewServeMux()
	httpMux.Handle("GET /bank/admin/exchanges/test-mode", marketModeHTTPHandler) // GET /bank/admin/exchanges/test-mode
	httpMux.Handle("/bank/payments/", receiptHandler)                           // GET /bank/payments/{id}/receipt
	httpMux.Handle("/bank/client/exchange-transfers", exchangeTransferHandler) // POST /bank/client/exchange-transfers
	httpMux.Handle("/bank/exchange-rates", exchangeRateHandler)                // GET /bank/exchange-rates[?from=X&to=Y&amount=Z]
	httpMux.Handle("/bank/exchange-rates/execute", exchangeRateHandler)        // POST /bank/exchange-rates/execute
	httpMux.Handle("POST /bank/cards/request", karticaRequestHandler)           // POST /bank/cards/request (Flow 2 Korak 1)
	httpMux.Handle("POST /bank/cards/confirm", karticaRequestHandler)           // POST /bank/cards/confirm (Flow 2 Korak 2)
	httpMux.Handle("GET /bank/cards/my", klientKarticeHandler)                  // GET  /bank/cards/my (klijentske kartice)
	httpMux.Handle("PATCH /bank/cards/{id}/block", klientKarticeHandler)        // PATCH /bank/cards/{id}/block (blokiranje)
	httpMux.Handle("/bank/internal/actuary/", internalActuaryHandler)           // POST/DELETE — user-service interni pozivi
	httpMux.Handle("GET /bank/trading/my-orders", myOrdersHandler)               // GET /bank/trading/my-orders — caller's own orders (all roles)
	httpMux.Handle("/bank/portfolio/", portfolioHandler)                        // GET /bank/portfolio/my, POST /bank/portfolio/publish, POST /bank/portfolio/exercise
	httpMux.Handle("/bank/tax/", taxHandler)                                    // GET /bank/tax/users, POST /bank/tax/calculate
	httpMux.Handle("/bank/funds/", fundHandler)                                 // GET /bank/funds, POST /bank/funds/{id}/invest, POST /bank/funds/{id}/withdraw
	httpMux.Handle("/bank/funds", fundHandler)                                  // GET /bank/funds (without trailing slash)
	httpMux.Handle("/", gwMux)                                                  // sve ostalo → gRPC-Gateway

	gatewaySrv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      httpMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// ── 7. Start InstallmentWorker (cron job) ────────────────────────────────
	// Worker koristi isti ctx koji se otkazuje pri SIGINT/SIGTERM,
	// što garantuje graceful shutdown bez dodatne sinhronizacije.
	go installmentWorker.Start(ctx)

	// ── 7e. Start TradingEngine (async order execution + fund settlement) ─────
	go tradingEngine.Start(ctx)

	// ── 7c. Start ListingRefresherWorker (osvežava cene hartija periodično) ────
	listingRefreshInterval := time.Duration(cfg.ListingRefreshIntervalMinutes) * time.Minute
	listingRefresherWorker := worker.NewListingRefresherWorker(listingRepo, listingRefreshInterval, cfg.FinnhubAPIKey, cfg.AlphaVantageAPIKey)
	go listingRefresherWorker.Start(ctx)

	// ── 7d. Start DailyLimitResetWorker (resetuje used_limit agenata u 23:59) ─
	dailyLimitResetWorker := worker.NewDailyLimitResetWorker(actuaryService)
	go dailyLimitResetWorker.Start(ctx)

	// ── 7b. Start ActuaryConsumer (RabbitMQ event listener) ──────────────────
	// Listens on the user_created queue and auto-provisions actuary profiles
	// for SUPERVISOR and AGENT employees created in user-service.
	if cfg.RabbitMQURL != "" {
		go worker.StartActuaryConsumer(ctx, cfg.RabbitMQURL, actuaryRepo)
	} else {
		log.Printf("[main] RABBITMQ_URL nije postavljen — ActuaryConsumer neće biti pokrenut")
	}

	// ── 8. Start gRPC server ─────────────────────────────────────────────────
	go func() {
		if err := grpcSrv.Serve(); err != nil {
			log.Fatalf("[grpc] serve error: %v", err)
		}
	}()

	// ── 9. Start gRPC-Gateway HTTP server ────────────────────────────────────
	go func() {
		log.Printf("[gateway] HTTP listening on %s → gRPC %s", cfg.HTTPAddr, grpcLocalTarget)
		if err := gatewaySrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[gateway] ListenAndServe error: %v", err)
		}
	}()

	// ── 9. Graceful shutdown on SIGINT / SIGTERM ──────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("[main] shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := gatewaySrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[gateway] shutdown error: %v", err)
	}

	grpcSrv.Stop()
	log.Println("[main] clean shutdown complete")
}
