// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/http/healthz"
	"github.com/milvus-io/milvus/pkg/eventlog"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
)

const (
	DefaultListenPort = "9091"
	ListenPortEnvKey  = "METRICS_PORT"
)

var (
	metricsServer *http.ServeMux
	server        *http.Server
)

// Provide alias for native http package
// avoiding import alias when using http package

type (
	ResponseWriter = http.ResponseWriter
	Request        = http.Request
)

type Handler struct {
	Path        string
	HandlerFunc http.HandlerFunc
	Handler     http.Handler
}

func registerDefaults() {
	Register(&Handler{
		Path: LogLevelRouterPath,
		HandlerFunc: func(w http.ResponseWriter, req *http.Request) {
			log.Level().ServeHTTP(w, req)
		},
	})
	Register(&Handler{
		Path:    HealthzRouterPath,
		Handler: healthz.Handler(),
	})
	Register(&Handler{
		Path:    EventLogRouterPath,
		Handler: eventlog.Handler(),
	})
}

func Register(h *Handler) {
	if metricsServer == nil {
		if paramtable.Get().HTTPCfg.EnablePprof.GetAsBool() {
			metricsServer = http.DefaultServeMux
		} else {
			metricsServer = http.NewServeMux()
		}
	}
	if h.HandlerFunc != nil {
		metricsServer.HandleFunc(h.Path, h.HandlerFunc)
		return
	}
	if h.Handler != nil {
		metricsServer.Handle(h.Path, h.Handler)
	}
}

func ServeHTTP() {
	registerDefaults()
	go func() {
		bindAddr := getHTTPAddr()
		log.Info("management listen", zap.String("addr", bindAddr))
		server = &http.Server{Handler: metricsServer, Addr: bindAddr, ReadTimeout: 10 * time.Second}
		if err := server.ListenAndServe(); err != nil {
			log.Error("handle metrics failed", zap.Error(err))
		}
	}()
}

func getHTTPAddr() string {
	port := os.Getenv(ListenPortEnvKey)
	_, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Sprintf(":%s", DefaultListenPort)
	}
	paramtable.Get().Save(paramtable.Get().CommonCfg.MetricsPort.Key, port)

	return fmt.Sprintf(":%s", port)
}