package service

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/mailgun/vulcand/Godeps/_workspace/src/github.com/gorilla/mux"
	"github.com/mailgun/vulcand/Godeps/_workspace/src/github.com/mailgun/go-etcd/etcd"
	log "github.com/mailgun/vulcand/Godeps/_workspace/src/github.com/mailgun/gotools-log"
	runtime "github.com/mailgun/vulcand/Godeps/_workspace/src/github.com/mailgun/gotools-runtime"
	timetools "github.com/mailgun/vulcand/Godeps/_workspace/src/github.com/mailgun/gotools-time"

	"github.com/mailgun/vulcand/api"
	"github.com/mailgun/vulcand/backend"
	"github.com/mailgun/vulcand/backend/etcdbackend"
	"github.com/mailgun/vulcand/configure"
	"github.com/mailgun/vulcand/plugin"
	"github.com/mailgun/vulcand/secret"
	"github.com/mailgun/vulcand/server"
)

func Run(registry *plugin.Registry) error {
	options, err := ParseCommandLine()
	if err != nil {
		return fmt.Errorf("Failed to parse command line: %s", err)
	}
	service := NewService(options, registry)
	if err := service.Start(); err != nil {
		return fmt.Errorf("Service exited with error: %s", err)
	} else {
		log.Infof("Service exited gracefully")
	}
	return nil
}

type Service struct {
	client       *etcd.Client
	backend      backend.Backend
	options      Options
	registry     *plugin.Registry
	apiRouter    *mux.Router
	errorC       chan error
	sigC         chan os.Signal
	configurator *configure.Configurator
}

func NewService(options Options, registry *plugin.Registry) *Service {
	return &Service{
		registry: registry,
		options:  options,
		errorC:   make(chan error),
		sigC:     make(chan os.Signal),
	}
}

func (s *Service) Start() error {
	log.Init([]*log.LogConfig{&log.LogConfig{Name: s.options.Log}})

	var box *secret.Box
	if s.options.BoxKey != "" {
		key, err := secret.KeyFromString(s.options.BoxKey)
		if err != nil {
			return err
		}
		b, err := secret.NewBox(key)
		if err != nil {
			return err
		}
		box = b
	}

	b, err := etcdbackend.NewEtcdBackendWithOptions(
		s.registry, s.options.EtcdNodes, s.options.EtcdKey,
		etcdbackend.Options{
			EtcdConsistency: s.options.EtcdConsistency,
			Box:             box,
		})
	if err != nil {
		return err
	}
	s.backend = b

	if s.options.PidPath != "" {
		if err := runtime.WritePid(s.options.PidPath); err != nil {
			return fmt.Errorf("failed to write PID file: %v\n", err)
		}
	}

	serverMaker := func() (server.Server, error) {
		return server.NewMuxServerWithOptions(server.Options{
			DialTimeout:    s.options.EndpointDialTimeout,
			ReadTimeout:    s.options.ServerReadTimeout,
			WriteTimeout:   s.options.ServerWriteTimeout,
			MaxHeaderBytes: s.options.ServerMaxHeaderBytes,
			DefaultListener: &backend.Listener{
				Id:       "DefaultListener",
				Protocol: "http",
				Address: backend.Address{
					Network: "tcp",
					Address: fmt.Sprintf("%s:%d", s.options.Interface, s.options.Port),
				},
			},
		})
	}

	s.configurator = configure.NewConfigurator(serverMaker, b, s.errorC, &timetools.RealTime{})

	// Tells configurator to perform initial proxy configuration and start watching changes
	if err := s.configurator.Start(); err != nil {
		return err
	}

	if err := s.initApi(); err != nil {
		return err
	}

	go func() {
		s.errorC <- s.startApi()
	}()

	signal.Notify(s.sigC, os.Interrupt, os.Kill, syscall.SIGTERM)

	// Block until a signal is received or we got an error
	select {
	case signal := <-s.sigC:
		if signal == syscall.SIGTERM {
			log.Infof("Got signal %s, shutting down gracefully", signal)
			s.configurator.Stop(true)
			log.Infof("All servers stopped")
		} else {
			log.Infof("Got signal %s, exiting now without waiting", signal)
			s.configurator.Stop(false)
		}
		return nil
	case err := <-s.errorC:
		log.Infof("Got request to shutdown with error: %s", err)
		return err
	}
	return nil
}

func (s *Service) initApi() error {
	s.apiRouter = mux.NewRouter()
	api.InitProxyController(s.backend, s.configurator, s.configurator.GetConnWatcher(), s.apiRouter)
	return nil
}

func (s *Service) startApi() error {
	addr := fmt.Sprintf("%s:%d", s.options.ApiInterface, s.options.ApiPort)

	server := &http.Server{
		Addr:           addr,
		Handler:        s.apiRouter,
		ReadTimeout:    s.options.ServerReadTimeout,
		WriteTimeout:   s.options.ServerWriteTimeout,
		MaxHeaderBytes: 1 << 20,
	}
	return server.ListenAndServe()
}
