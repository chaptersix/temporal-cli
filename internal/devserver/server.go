// The MIT License
//
// Copyright (c) 2022 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Copyright (c) 2021 Datadog, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package devserver

import (
	"fmt"
	"log/slog"

	uiserver "github.com/temporalio/ui-server/v2/server"
	uiconfig "github.com/temporalio/ui-server/v2/server/config"
	uiserveroptions "github.com/temporalio/ui-server/v2/server/server_options"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/server/temporal"
	serverdevserver "go.temporal.io/server/tools/devserver"
)

type StartOptions struct {
	// Required fields
	FrontendIP             string
	FrontendPort           int
	Namespaces             []string
	ClusterID              string
	MasterClusterName      string
	CurrentClusterName     string
	InitialFailoverVersion int
	Logger                 *slog.Logger
	LogLevel               slog.Level

	// Optional fields
	UIIP                  string // Empty means no UI
	UIPort                int    // Required if UIIP is non-empty
	UIAssetPath           string
	UICodecEndpoint       string
	PublicPath            string
	DatabaseFile          string
	MetricsPort           int
	PProfPort             int
	SqlitePragmas         map[string]string
	FrontendHTTPPort      int
	EnableGlobalNamespace bool
	DynamicConfigValues   map[string]any
	SearchAttributes      map[string]enums.IndexedValueType
	LogConfig             func([]byte)
}

type Server struct {
	server   temporal.Server
	ui       *uiserver.Server
	logLevel *slog.LevelVar
}

func Start(options StartOptions) (*Server, error) {
	// Validate CLI-specific options (server validates its own options)
	if options.Logger == nil {
		return nil, fmt.Errorf("missing logger")
	}
	if options.UIIP != "" && options.UIPort == 0 {
		return nil, fmt.Errorf("must provide UI port if UI IP is provided")
	}

	// Build UI server if needed
	var ui *uiserver.Server
	if options.UIIP != "" {
		ui = options.buildUIServer()
	}

	// Create logger wrapper
	logLevel := &slog.LevelVar{}
	logLevel.Set(options.LogLevel)
	logger := slogLogger{
		log:   options.Logger,
		level: logLevel,
	}

	// Build server options for the server repo's devserver
	serverOpts := serverdevserver.Options{
		FrontendIP:             options.FrontendIP,
		FrontendPort:           options.FrontendPort,
		Namespaces:             options.Namespaces,
		ClusterID:              options.ClusterID,
		MasterClusterName:      options.MasterClusterName,
		CurrentClusterName:     options.CurrentClusterName,
		InitialFailoverVersion: options.InitialFailoverVersion,
		Logger:                 logger,
		DatabaseFile:           options.DatabaseFile,
		MetricsPort:            options.MetricsPort,
		PProfPort:              options.PProfPort,
		SqlitePragmas:          options.SqlitePragmas,
		FrontendHTTPPort:       options.FrontendHTTPPort,
		EnableGlobalNamespace:  options.EnableGlobalNamespace,
		DynamicConfigValues:    options.DynamicConfigValues,
		SearchAttributes:       options.SearchAttributes,
		LogConfig:              options.LogConfig,
	}

	// Create the temporal server using the server repo's devserver
	server, err := serverdevserver.New(serverOpts)
	if err != nil {
		return nil, fmt.Errorf("failed creating server: %w", err)
	}

	// Start. We have to start UI server in background because its start call is
	// blocking. Therefore we have no way to relay error out to users, so we just
	// log and panic.
	if ui != nil {
		go func() {
			if err := ui.Start(); err != nil {
				options.Logger.Error("failed running UI server", "error", err)
				panic(err)
			}
		}()
	}
	if err := server.Start(); err != nil {
		// Stop UI before returning to avoid leaks
		if ui != nil {
			ui.Stop()
		}
		return nil, err
	}
	return &Server{server: server, ui: ui, logLevel: logLevel}, nil
}

func (s *Server) Stop() {
	if s.ui != nil {
		s.ui.Stop()
	}
	s.server.Stop()
}

func (s *Server) SuppressWarnings() {
	if s.logLevel != nil {
		s.logLevel.Set(slog.LevelError)
	}
}

func (s *StartOptions) buildUIServer() *uiserver.Server {
	return uiserver.NewServer(uiserveroptions.WithConfigProvider(&uiconfig.Config{
		Host:                MaybeEscapeIPv6(s.UIIP),
		Port:                s.UIPort,
		TemporalGRPCAddress: fmt.Sprintf("%v:%v", MaybeEscapeIPv6(s.FrontendIP), s.FrontendPort),
		EnableUI:            true,
		PublicPath:          s.PublicPath,
		UIAssetPath:         s.UIAssetPath,
		Codec:               uiconfig.Codec{Endpoint: s.UICodecEndpoint},
		CORS:                uiconfig.CORS{CookieInsecure: true},
		HideLogs:            true,
	}))
}
