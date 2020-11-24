// Copyright 2019 HAProxy Technologies LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/haproxytech/client-native/v2/configuration"
	parser "github.com/haproxytech/config-parser/v3"
	"github.com/haproxytech/config-parser/v3/params"
	"github.com/haproxytech/config-parser/v3/types"
	"github.com/haproxytech/models/v2"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/haproxytech/kubernetes-ingress/controller/haproxy/api"
	ingressRoute "github.com/haproxytech/kubernetes-ingress/controller/ingress"
	"github.com/haproxytech/kubernetes-ingress/controller/store"
	"github.com/haproxytech/kubernetes-ingress/controller/utils"
)

const (
	// Configmaps
	Main        = "main"
	TCPServices = "tcpservices"
	Errorfiles  = "errorfiles"
	//sections
	FrontendHTTP       = "http"
	FrontendHTTPS      = "https"
	FrontendSSL        = "ssl"
	HTTPDefaultBackend = "http_default"
	SSLDefaultBaceknd  = "ssl"
	//Status
	ADDED    = store.ADDED
	DELETED  = store.DELETED
	ERROR    = store.ERROR
	EMPTY    = store.EMPTY
	MODIFIED = store.MODIFIED
)

var (
	HAProxyBinary        string
	HAProxyCFG           string
	HAProxyCfgDir        string
	HAProxyCertDir       string
	HAProxyStateDir      string
	HAProxyMapDir        string
	HAProxyErrFileDir    string
	HAProxyRuntimeSocket string
	HAProxyPIDFile       string
	TransactionDir       string
)

var logger = utils.GetLogger()

// HAProxyController is ingress controller
type HAProxyController struct {
	k8s            *K8s
	Store          store.K8s
	PublishService *store.Service
	IngressClass   string
	cfg            Configuration
	osArgs         utils.OSArgs
	Client         api.HAProxyClient
	eventChan      chan SyncDataEvent
	serverlessPods map[string]int
	UpdateHandlers []UpdateHandler
}

// Wrapping a Native-Client transaction and commit it.
// Returning an error to let panic or log it upon the scenario.
func (c *HAProxyController) clientClosure(fn func()) (err error) {
	if err = c.Client.APIStartTransaction(); err != nil {
		return
	}
	fn()
	if err = c.Client.APICommitTransaction(); err != nil {
		return
	}
	c.Client.APIDisposeTransaction()
	return
}

// Start initializes and runs HAProxyController
func (c *HAProxyController) Start(ctx context.Context, osArgs utils.OSArgs) {

	c.osArgs = osArgs

	logger.SetLevel(osArgs.LogLevel.LogLevel)
	c.haproxyInitialize()
	c.initHandlers()

	// handling dynamic frontend binding
	{
		var err error
		var http, https bool

		p := &parser.Parser{
			Options: parser.Options{
				UseV2HTTPCheck: true,
			},
		}
		logger.Panic(p.LoadData(HAProxyCFG))

		if !c.osArgs.DisableHTTP {
			http, err = c.handleBind(p, "http", c.osArgs.HTTPBindPort)
		}
		if !c.osArgs.DisableHTTPS {
			https, err = c.handleBind(p, "https", c.osArgs.HTTPSBindPort)
		}

		if err == nil && (http || https) {
			err = p.Save(HAProxyCFG)
		}

		logger.Panic(err)
	}
	if c.osArgs.PprofEnabled {
		logger.Error(c.clientClosure(func() {
			logger.Error(c.handlePprof())
		}))
	}

	parts := strings.Split(osArgs.PublishService, "/")
	if len(parts) == 2 {
		c.PublishService = &store.Service{
			Namespace: parts[0],
			Name:      parts[1],
			Status:    EMPTY,
			Addresses: []string{},
		}
	}

	var k8s *K8s
	var err error

	if osArgs.OutOfCluster {
		kubeconfig := filepath.Join(utils.HomeDir(), ".kube", "config")
		if osArgs.KubeConfig != "" {
			kubeconfig = osArgs.KubeConfig
		}
		k8s, err = GetRemoteKubernetesClient(kubeconfig)
	} else {
		k8s, err = GetKubernetesClient()
	}
	if err != nil {
		logger.Panic(err)
	}
	c.k8s = k8s

	x := k8s.API.Discovery()
	if k8sVersion, err := x.ServerVersion(); err != nil {
		logger.Panicf("Unable to get Kubernetes version: %v\n", err)
	} else {
		logger.Infof("Running on Kubernetes version: %s %s", k8sVersion.String(), k8sVersion.Platform)
	}

	// Starting from Kubernetes 1.19 a valid IngressClass resource must be used:
	// checking if the provided one is correctly registered with the current
	// HAProxy Ingress Controller instance.
	if len(c.IngressClass) > 0 {
		logger.Panic(c.k8s.IsMatchingSelectedIngressClass(c.IngressClass))
	}

	c.serverlessPods = map[string]int{}
	c.eventChan = make(chan SyncDataEvent, watch.DefaultChanSize*6)
	go c.monitorChanges()
	<-ctx.Done()
}

// updateHAProxy syncs HAProxy configuration
func (c *HAProxyController) updateHAProxy() error {
	logger.Trace("HAProxy config sync started")
	reload := false

	err := c.Client.APIStartTransaction()
	if err != nil {
		logger.Error(err)
		return err
	}
	defer func() {
		c.Client.APIDisposeTransaction()
	}()

	restart, reload := c.handleGlobalAnnotations()

	c.handleDefaultService()

	usedCerts := map[string]struct{}{}
	c.cfg.UsedCerts = usedCerts

	for _, namespace := range c.Store.Namespaces {
		if !namespace.Relevant {
			continue
		}
		for _, ingress := range namespace.Ingresses {
			if c.PublishService != nil && ingress.Status != DELETED {
				logger.Error(c.k8s.UpdateIngressStatus(ingress, c.PublishService))
			}
			// handle Default Backend
			if ingress.DefaultBackend != nil {
				c.cfg.IngressRoutes.AddRoute(&ingressRoute.Route{
					Namespace: namespace,
					Ingress:   ingress,
					Path:      ingress.DefaultBackend,
				})
			}
			// handle Ingress rules
			for _, rule := range ingress.Rules {
				for _, path := range rule.Paths {
					c.cfg.IngressRoutes.AddRoute(&ingressRoute.Route{
						Namespace:      namespace,
						Ingress:        ingress,
						Host:           rule.Host,
						Path:           path,
						SSLPassthrough: c.sslPassthroughEnabled(namespace, ingress, path),
					})
				}
			}
			//handle certs
			ingressSecrets := map[string]struct{}{}
			for _, tls := range ingress.TLS {
				if _, ok := ingressSecrets[tls.SecretName.Value]; !ok {
					ingressSecrets[tls.SecretName.Value] = struct{}{}
					reload = c.handleTLSSecret(*ingress, *tls, usedCerts) || reload
				}
			}

			// Ingress Annotations
			if len(ingress.Rules) == 0 {
				logger.Debugf("Ingress %s/%s: no rules defined", ingress.Namespace, ingress.Name)
				continue
			}
			c.handleIngressAnnotations(ingress)
		}
	}

	for _, handler := range c.UpdateHandlers {
		r, errHandler := handler.Update(c.Store, &c.cfg, c.Client)
		logger.Error(errHandler)
		reload = reload || r
	}

	err = c.Client.APICommitTransaction()
	if err != nil {
		logger.Error(err)
		return err
	}
	c.clean()
	if restart {
		if err := c.haproxyService("restart"); err != nil {
			logger.Error(err)
		} else {
			logger.Info("HAProxy restarted")
		}
		return nil
	}
	if reload {
		if err := c.haproxyService("reload"); err != nil {
			logger.Error(err)
		} else {
			logger.Info("HAProxy reloaded")
		}
	}

	logger.Trace("HAProxy config sync terminated")
	return nil
}

// haproxyInitialize initializes HAProxy environment and its API client.
func (c *HAProxyController) haproxyInitialize() {
	var err error
	// HAProxy executable
	HAProxyBinary = "/usr/local/sbin/haproxy"
	if c.osArgs.Program != "" {
		HAProxyBinary = c.osArgs.Program
	}
	_, err = os.Stat(HAProxyBinary)
	if err != nil && !c.osArgs.Test {
		logger.Panic(err)
	}
	// Initialize files and directories
	if HAProxyCFG == "" {
		HAProxyCFG = filepath.Join(HAProxyCfgDir, "haproxy.cfg")
	}
	if _, err = os.Stat(HAProxyCFG); err != nil {
		logger.Panic(err)
	}
	if HAProxyPIDFile == "" {
		HAProxyPIDFile = "/var/run/haproxy.pid"
	}
	if HAProxyRuntimeSocket == "" {
		HAProxyRuntimeSocket = "/var/run/haproxy-runtime-api.sock"
	}
	if HAProxyCertDir == "" {
		HAProxyCertDir = filepath.Join(HAProxyCfgDir, "certs")
	}
	if HAProxyMapDir == "" {
		HAProxyMapDir = filepath.Join(HAProxyCfgDir, "maps")
	}
	if HAProxyErrFileDir == "" {
		HAProxyErrFileDir = filepath.Join(HAProxyCfgDir, "errors")
	}
	if HAProxyStateDir == "" {
		HAProxyStateDir = "/var/state/haproxy/"
	}
	if TransactionDir != "" {
		err = os.MkdirAll(TransactionDir, 0755)
		if err != nil {
			logger.Panic(err)
		}
	}
	for _, d := range []string{HAProxyCertDir, HAProxyMapDir, HAProxyErrFileDir, HAProxyStateDir} {
		err = os.MkdirAll(d, 0755)
		if err != nil {
			logger.Panic(err)
		}
	}
	_, err = os.Create(filepath.Join(HAProxyStateDir, "global"))
	logger.Err(err)

	// Initialize HAProxy client API
	c.Client, err = api.Init(TransactionDir, HAProxyCFG, "haproxy", HAProxyRuntimeSocket)
	if err != nil {
		logger.Panic(err)
	}
	if c.osArgs.OutOfCluster && !c.osArgs.Test {
		logger.Panic(c.clientClosure(func() {
			var errors utils.Errors
			errors.Add(
				// Configure runtime socket
				c.Client.RuntimeSocket(nil),
				c.Client.RuntimeSocket(&types.Socket{
					Path: HAProxyRuntimeSocket,
					Params: []params.BindOption{
						&params.BindOptionDoubleWord{Name: "expose-fd", Value: "listeners"},
						&params.BindOptionValue{Name: "level", Value: "admin"},
					},
				}),
				// Configure pidfile
				c.Client.PIDFile(&types.StringC{Value: HAProxyPIDFile}),
				// Configure server-state-base
				c.Client.ServerStateBase(&types.StringC{Value: HAProxyStateDir}),
			)
			if errors.Result() != nil {
				logger.Panic(errors.Result())
			}
		}))
	}

	cmd := exec.Command("sh", "-c", "haproxy -v")
	haproxyInfo, err := cmd.Output()
	if err == nil {
		haproxyInfo := strings.Split(string(haproxyInfo), "\n")
		logger.Printf("Running with %s", haproxyInfo[0])
	} else {
		logger.Error(err)
	}

	logger.Infof("Starting HAProxy with %s", HAProxyCFG)
	logger.Panic(c.haproxyService("start"))

	hostname, err := os.Hostname()
	logger.Error(err)
	logger.Infof("Running on %s", hostname)

	c.cfg.Init(HAProxyMapDir)
}

// handleBind configures Frontends bind lines
func (c *HAProxyController) handleBind(p *parser.Parser, protocol string, port int64) (reload bool, err error) {
	var binds []models.Bind
	if !c.osArgs.DisableIPV4 {
		binds = append(binds, models.Bind{
			Name:    "bind_1",
			Address: c.osArgs.IPV4BindAddr,
			Port:    utils.PtrInt64(port),
		})
	}
	if !c.osArgs.DisableIPV6 {
		binds = append(binds, models.Bind{
			Name:    "bind_2",
			Address: c.osArgs.IPV6BindAddr,
			Port:    utils.PtrInt64(port),
			V4v6:    true,
		})
	}
	for i, b := range binds {
		if err = p.Insert(parser.Frontends, protocol, "bind", configuration.SerializeBind(b), i+1); err != nil {
			return false, fmt.Errorf("cannot create bind %s for protocol %s: %s", b.Name, protocol, err.Error())
		}
	}
	reload = len(binds) > 0
	if reload {
		err = p.Delete(parser.Frontends, protocol, "bind", 0)
	}
	return
}

// handlePprof enables  pprof backend
func (c *HAProxyController) handlePprof() (err error) {
	pprofBackend := "pprof"

	err = c.Client.BackendCreate(models.Backend{
		Name: pprofBackend,
		Mode: "http",
	})
	if err != nil {
		return err
	}
	err = c.Client.BackendServerCreate(pprofBackend, models.Server{
		Name:    "pprof",
		Address: "127.0.0.1:6060",
	})
	if err != nil {
		return err
	}
	logger.Debug("pprof backend created")
	c.cfg.IngressRoutes.AddRoute(&ingressRoute.Route{
		Path: &store.IngressPath{
			Path:           "/debug/pprof",
			ExactPathMatch: false,
		},
		BackendName: pprofBackend,
	})
	return nil
}

// handleDefaultService configures HAProy default backend provided via cli param "default-backend-service"
func (c *HAProxyController) handleDefaultService() {
	dsvcData, _ := c.Store.GetValueFromAnnotations("default-backend-service")
	dsvc := strings.Split(dsvcData.Value, "/")

	if len(dsvc) != 2 {
		logger.Errorf("default service invalid data")
		return
	}
	if dsvc[0] == "" || dsvc[1] == "" {
		return
	}
	namespace, ok := c.Store.Namespaces[dsvc[0]]
	if !ok {
		logger.Errorf("default service invalid namespace " + dsvc[0])
		return
	}
	service, ok := namespace.Services[dsvc[1]]
	if !ok {
		logger.Errorf("service '" + dsvc[1] + "' does not exist")
		return
	}
	ingress := &store.Ingress{
		Namespace:   namespace.Name,
		Name:        "DefaultService",
		Annotations: store.MapStringW{},
		Rules:       map[string]*store.IngressRule{},
	}
	path := &store.IngressPath{
		ServiceName:      service.Name,
		ServicePortInt:   service.Ports[0].Port,
		IsDefaultBackend: true,
	}
	c.cfg.IngressRoutes.AddRoute(&ingressRoute.Route{
		Namespace: namespace,
		Ingress:   ingress,
		Path:      path,
	})
}

// clean controller state
func (c *HAProxyController) clean() {
	c.Store.Clean()
	c.cfg.Clean()
	if c.PublishService != nil {
		c.PublishService.Status = EMPTY
	}
	c.cfg.SSLPassthrough = false
}

func (c *HAProxyController) sslPassthroughEnabled(namespace *store.Namespace, ingress *store.Ingress, path *store.IngressPath) bool {
	var annSSLPassthrough *store.StringW
	service, ok := namespace.Services[path.ServiceName]
	if ok {
		annSSLPassthrough, _ = c.Store.GetValueFromAnnotations("ssl-passthrough", service.Annotations, ingress.Annotations, c.Store.ConfigMaps[Main].Annotations)
	} else {
		annSSLPassthrough, _ = c.Store.GetValueFromAnnotations("ssl-passthrough", ingress.Annotations, c.Store.ConfigMaps[Main].Annotations)
	}
	enabled, err := utils.GetBoolValue(annSSLPassthrough.Value, "ssl-passthrough")
	if err != nil {
		logger.Errorf("ssl-passthrough annotation: %s", err)
		return false
	}
	if annSSLPassthrough.Status == DELETED {
		return false
	}
	if enabled {
		c.cfg.SSLPassthrough = true
		return true
	}
	return false
}
