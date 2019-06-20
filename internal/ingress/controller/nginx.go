/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/golang/glog"

	apiv1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/kubernetes/pkg/util/filesystem"

	"k8s.io/ingress-nginx/internal/file"
	"k8s.io/ingress-nginx/internal/ingress"
	"k8s.io/ingress-nginx/internal/ingress/annotations"
	"k8s.io/ingress-nginx/internal/ingress/annotations/class"
	ngx_config "k8s.io/ingress-nginx/internal/ingress/controller/config"
	"k8s.io/ingress-nginx/internal/ingress/controller/process"
	ngx_template "k8s.io/ingress-nginx/internal/ingress/controller/template"
	"k8s.io/ingress-nginx/internal/ingress/defaults"
	"k8s.io/ingress-nginx/internal/ingress/status"
	"k8s.io/ingress-nginx/internal/ingress/store"
	ing_net "k8s.io/ingress-nginx/internal/net"
	"k8s.io/ingress-nginx/internal/net/dns"
	"k8s.io/ingress-nginx/internal/net/ssl"
	"k8s.io/ingress-nginx/internal/task"
	"k8s.io/ingress-nginx/internal/watch"
)

type statusModule string

const (
	ngxHealthPath = "/healthz"

	defaultStatusModule statusModule = "default"
	vtsStatusModule     statusModule = "vts"
)

var (
	tmplPath    = "/etc/nginx/template/nginx.tmpl"
	cfgPath     = "/etc/nginx/nginx.conf"
	nginxBinary = "/usr/sbin/nginx"
)

// NewNGINXController creates a new NGINX Ingress controller.
// If the environment variable NGINX_BINARY exists it will be used
// as source for nginx commands
func NewNGINXController(config *Configuration, fs file.Filesystem) *NGINXController {
	ngx := os.Getenv("NGINX_BINARY")
	if ngx == "" {
		ngx = nginxBinary
	}

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{
		Interface: config.Client.CoreV1().Events(config.Namespace),
	})

	h, err := dns.GetSystemNameServers()
	if err != nil {
		glog.Warningf("unexpected error reading system nameservers: %v", err)
	}

	n := &NGINXController{
		backendDefaults: ngx_config.NewDefault().Backend,
		binary:          ngx,

		configmap: &apiv1.ConfigMap{},

		isIPV6Enabled: ing_net.IsIPv6Enabled(),

		resolver:        h,
		cfg:             config,
		sslCertTracker:  store.NewSSLCertTracker(),
		syncRateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.3, 1),

		recorder: eventBroadcaster.NewRecorder(scheme.Scheme, apiv1.EventSource{
			Component: "nginx-ingress-controller",
		}),

		stopCh:   make(chan struct{}),
		stopLock: &sync.Mutex{},

		fileSystem: fs,

		// create an empty configuration.
		runningConfig: &ingress.Configuration{},
	}

	n.listers, n.controllers = n.createListers(n.stopCh)

	n.stats = newStatsCollector(config.Namespace, class.IngressClass, n.binary, n.cfg.ListenPorts.Status)

	n.syncQueue = task.NewTaskQueue(n.syncIngress)

	n.annotations = annotations.NewAnnotationExtractor(n)

	if config.UpdateStatus {
		n.syncStatus = status.NewStatusSyncer(status.Config{
			Client:                 config.Client,
			PublishService:         config.PublishService,
			IngressLister:          n.listers.Ingress,
			ElectionID:             config.ElectionID,
			IngressClass:           class.IngressClass,
			DefaultIngressClass:    class.DefaultClass,
			UpdateStatusOnShutdown: config.UpdateStatusOnShutdown,
			UseNodeInternalIP:      config.UseNodeInternalIP,
		})
	} else {
		glog.Warning("Update of ingress status is disabled (flag --update-status=false was specified)")
	}

	var onChange func()
	onChange = func() {
		template, err := ngx_template.NewTemplate(tmplPath, fs)
		if err != nil {
			// this error is different from the rest because it must be clear why nginx is not working
			glog.Errorf(`
-------------------------------------------------------------------------------
Error loading new template : %v
-------------------------------------------------------------------------------
`, err)
			return
		}

		n.t = template
		glog.Info("new NGINX template loaded")
		n.SetForceReload(true)
	}

	ngxTpl, err := ngx_template.NewTemplate(tmplPath, fs)
	if err != nil {
		glog.Fatalf("invalid NGINX template: %v", err)
	}

	n.t = ngxTpl

	// TODO: refactor
	if _, ok := fs.(filesystem.DefaultFs); !ok {
		watch.NewDummyFileWatcher(tmplPath, onChange)
	} else {
		_, err = watch.NewFileWatcher(tmplPath, onChange)
		if err != nil {
			glog.Fatalf("unexpected error watching template %v: %v", tmplPath, err)
		}
	}

	return n
}

// NGINXController ...
type NGINXController struct {
	cfg *Configuration

	listers     *ingress.StoreLister
	controllers *cacheController

	annotations annotations.Extractor

	recorder record.EventRecorder

	syncQueue *task.Queue

	syncStatus status.Sync

	// local store of SSL certificates
	// (only certificates used in ingress)
	sslCertTracker *store.SSLCertTracker

	syncRateLimiter flowcontrol.RateLimiter

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock *sync.Mutex

	stopCh chan struct{}

	// ngxErrCh channel used to detect errors with the nginx processes
	ngxErrCh chan error

	// runningConfig contains the running configuration in the Backend
	runningConfig *ingress.Configuration

	forceReload int32

	t *ngx_template.Template

	configmap *apiv1.ConfigMap

	binary   string
	resolver []net.IP

	stats        *statsCollector
	statusModule statusModule

	// returns true if IPV6 is enabled in the pod
	isIPV6Enabled bool

	// returns true if proxy protocol es enabled
	IsProxyProtocolEnabled bool

	isSSLPassthroughEnabled bool

	isShuttingDown bool

	Proxy *TCPProxy

	backendDefaults defaults.Backend

	fileSystem filesystem.Filesystem
}

// Start start a new NGINX master process running in foreground.
func (n *NGINXController) Start() {
	glog.Infof("starting Ingress controller")

	n.controllers.Run(n.stopCh)

	// initial sync of secrets to avoid unnecessary reloads
	glog.Info("running initial sync of secrets")
	for _, obj := range n.listers.Ingress.List() {
		ing := obj.(*extensions.Ingress)

		if !class.IsValid(ing) {
			a := ing.GetAnnotations()[class.IngressKey]
			glog.Infof("ignoring add for ingress %v based on annotation %v with value %v", ing.Name, class.IngressKey, a)
			continue
		}

		n.readSecrets(ing)
	}

	if n.cfg.EnableSSLChainCompletion {
		go wait.Until(n.checkSSLChainIssues, 60*time.Second, n.stopCh)
	}

	if n.syncStatus != nil {
		go n.syncStatus.Run(n.stopCh)
	}

	go wait.Until(n.checkMissingSecrets, 30*time.Second, n.stopCh)

	done := make(chan error, 1)
	cmd := exec.Command(n.binary, "-c", cfgPath)

	// put nginx in another process group to prevent it
	// to receive signals meant for the controller
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	glog.Info("starting NGINX process...")
	n.start(cmd)

	go n.syncQueue.Run(time.Second, n.stopCh)
	// force initial sync
	n.syncQueue.Enqueue(&extensions.Ingress{})

	for {
		select {
		case err := <-done:
			if n.isShuttingDown {
				break
			}

			// if the nginx master process dies the workers continue to process requests,
			// passing checks but in case of updates in ingress no updates will be
			// reflected in the nginx configuration which can lead to confusion and report
			// issues because of this behavior.
			// To avoid this issue we restart nginx in case of errors.
			if process.IsRespawnIfRequired(err) {
				process.WaitUntilPortIsAvailable(n.cfg.ListenPorts.HTTP)
				// release command resources
				cmd.Process.Release()
				cmd = exec.Command(n.binary, "-c", cfgPath)
				// start a new nginx master process if the controller is not being stopped
				n.start(cmd)
			}
		case <-n.stopCh:
			break
		}
	}
}

// Stop gracefully stops the NGINX master process.
func (n *NGINXController) Stop() error {
	n.isShuttingDown = true

	n.stopLock.Lock()
	defer n.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if n.syncQueue.IsShuttingDown() {
		return fmt.Errorf("shutdown already in progress")
	}

	glog.Infof("shutting down controller queues")
	close(n.stopCh)
	go n.syncQueue.Shutdown()
	if n.syncStatus != nil {
		n.syncStatus.Shutdown()
	}

	// Send stop signal to Nginx
	glog.Info("stopping NGINX process...")
	cmd := exec.Command(n.binary, "-c", cfgPath, "-s", "quit")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return err
	}

	// Wait for the Nginx process disappear
	timer := time.NewTicker(time.Second * 1)
	for range timer.C {
		if !process.IsNginxRunning() {
			glog.Info("NGINX process has stopped")
			timer.Stop()
			break
		}
	}

	return nil
}

func (n *NGINXController) start(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		glog.Fatalf("nginx error: %v", err)
		n.ngxErrCh <- err
		return
	}

	go func() {
		n.ngxErrCh <- cmd.Wait()
	}()
}

// DefaultEndpoint returns the default endpoint to be use as default server that returns 404.
func (n NGINXController) DefaultEndpoint() ingress.Endpoint {
	return ingress.Endpoint{
		Address: "127.0.0.1",
		Port:    fmt.Sprintf("%v", n.cfg.ListenPorts.Default),
		Target:  &apiv1.ObjectReference{},
	}
}

// testTemplate checks if the NGINX configuration inside the byte array is valid
// running the command "nginx -t" using a temporal file.
func (n NGINXController) testTemplate(cfg []byte) error {
	if len(cfg) == 0 {
		return fmt.Errorf("invalid nginx configuration (empty)")
	}
	tmpfile, err := ioutil.TempFile("", "nginx-cfg")
	if err != nil {
		return err
	}
	defer tmpfile.Close()
	err = ioutil.WriteFile(tmpfile.Name(), cfg, 0644)
	if err != nil {
		return err
	}
	out, err := exec.Command(n.binary, "-t", "-c", tmpfile.Name()).CombinedOutput()
	if err != nil {
		// this error is different from the rest because it must be clear why nginx is not working
		oe := fmt.Sprintf(`
-------------------------------------------------------------------------------
Error: %v
%v
-------------------------------------------------------------------------------
`, err, string(out))
		return errors.New(oe)
	}

	os.Remove(tmpfile.Name())
	return nil
}

// SetConfig sets the configured configmap
func (n *NGINXController) SetConfig(cmap *apiv1.ConfigMap) {
	n.configmap = cmap
	n.IsProxyProtocolEnabled = false

	m := map[string]string{}
	if cmap != nil {
		m = cmap.Data
	}

	val, ok := m["use-proxy-protocol"]
	if ok {
		b, err := strconv.ParseBool(val)
		if err == nil {
			n.IsProxyProtocolEnabled = b
		}
	}

	c := ngx_template.ReadConfig(m)
	if c.SSLSessionTicketKey != "" {
		d, err := base64.StdEncoding.DecodeString(c.SSLSessionTicketKey)
		if err != nil {
			glog.Warningf("unexpected error decoding key ssl-session-ticket-key: %v", err)
			c.SSLSessionTicketKey = ""
		}
		ioutil.WriteFile("/etc/nginx/tickets.key", d, 0644)
	}

	n.backendDefaults = c.Backend
}

// OnUpdate is called periodically by syncQueue to keep the configuration in sync.
//
// 1. converts configmap configuration to custom configuration object
// 2. write the custom template (the complexity depends on the implementation)
// 3. write the configuration file
//
// returning nill implies the backend will be reloaded.
// if an error is returned means requeue the update
func (n *NGINXController) OnUpdate(ingressCfg ingress.Configuration) error {
	cfg := ngx_template.ReadConfig(n.configmap.Data)
	cfg.Resolver = n.resolver

	servers := []*TCPServer{}
	for _, pb := range ingressCfg.PassthroughBackends {
		svc := pb.Service
		if svc == nil {
			glog.Warningf("missing service for PassthroughBackends %v", pb.Backend)
			continue
		}
		port, err := strconv.Atoi(pb.Port.String())
		if err != nil {
			for _, sp := range svc.Spec.Ports {
				if sp.Name == pb.Port.String() {
					port = int(sp.Port)
					break
				}
			}
		} else {
			for _, sp := range svc.Spec.Ports {
				if sp.Port == int32(port) {
					port = int(sp.Port)
					break
				}
			}
		}

		//TODO: Allow PassthroughBackends to specify they support proxy-protocol
		servers = append(servers, &TCPServer{
			Hostname:      pb.Hostname,
			IP:            svc.Spec.ClusterIP,
			Port:          port,
			ProxyProtocol: false,
		})
	}

	if n.isSSLPassthroughEnabled {
		n.Proxy.ServerList = servers
	}

	// we need to check if the status module configuration changed
	if cfg.EnableVtsStatus {
		n.setupMonitor(vtsStatusModule)
	} else {
		n.setupMonitor(defaultStatusModule)
	}

	// NGINX cannot resize the hash tables used to store server names.
	// For this reason we check if the defined size defined is correct
	// for the FQDN defined in the ingress rules adjusting the value
	// if is required.
	// https://trac.nginx.org/nginx/ticket/352
	// https://trac.nginx.org/nginx/ticket/631
	var longestName int
	var serverNameBytes int
	redirectServers := make(map[string]string)
	for _, srv := range ingressCfg.Servers {
		if longestName < len(srv.Hostname) {
			longestName = len(srv.Hostname)
		}
		serverNameBytes += len(srv.Hostname)
		if srv.RedirectFromToWWW {
			var n string
			if strings.HasPrefix(srv.Hostname, "www.") {
				n = strings.TrimLeft(srv.Hostname, "www.")
			} else {
				n = fmt.Sprintf("www.%v", srv.Hostname)
			}
			glog.V(3).Infof("creating redirect from %v to %v", srv.Hostname, n)
			if _, ok := redirectServers[n]; !ok {
				found := false
				for _, esrv := range ingressCfg.Servers {
					if esrv.Hostname == n {
						found = true
						break
					}
				}
				if !found {
					redirectServers[n] = srv.Hostname
				}
			}
		}
	}
	if cfg.ServerNameHashBucketSize == 0 {
		nameHashBucketSize := nginxHashBucketSize(longestName)
		glog.V(3).Infof("adjusting ServerNameHashBucketSize variable to %v", nameHashBucketSize)
		cfg.ServerNameHashBucketSize = nameHashBucketSize
	}
	serverNameHashMaxSize := nextPowerOf2(serverNameBytes)
	if cfg.ServerNameHashMaxSize < serverNameHashMaxSize {
		glog.V(3).Infof("adjusting ServerNameHashMaxSize variable to %v", serverNameHashMaxSize)
		cfg.ServerNameHashMaxSize = serverNameHashMaxSize
	}

	// the limit of open files is per worker process
	// and we leave some room to avoid consuming all the FDs available
	wp, err := strconv.Atoi(cfg.WorkerProcesses)
	glog.V(3).Infof("number of worker processes: %v", wp)
	if err != nil {
		wp = 1
	}
	maxOpenFiles := (sysctlFSFileMax() / wp) - 1024
	glog.V(3).Infof("maximum number of open file descriptors : %v", sysctlFSFileMax())
	if maxOpenFiles < 1024 {
		// this means the value of RLIMIT_NOFILE is too low.
		maxOpenFiles = 1024
	}

	setHeaders := map[string]string{}
	if cfg.ProxySetHeaders != "" {
		cmap, exists, err := n.listers.ConfigMap.GetByKey(cfg.ProxySetHeaders)
		if err != nil {
			glog.Warningf("unexpected error reading configmap %v: %v", cfg.ProxySetHeaders, err)
		}

		if exists {
			setHeaders = cmap.(*apiv1.ConfigMap).Data
		}
	}

	addHeaders := map[string]string{}
	if cfg.AddHeaders != "" {
		cmap, exists, err := n.listers.ConfigMap.GetByKey(cfg.AddHeaders)
		if err != nil {
			glog.Warningf("unexpected error reading configmap %v: %v", cfg.AddHeaders, err)
		}

		if exists {
			addHeaders = cmap.(*apiv1.ConfigMap).Data
		}
	}

	sslDHParam := ""
	if cfg.SSLDHParam != "" {
		secretName := cfg.SSLDHParam
		s, exists, err := n.listers.Secret.GetByKey(secretName)
		if err != nil {
			glog.Warningf("unexpected error reading secret %v: %v", secretName, err)
		}

		if exists {
			secret := s.(*apiv1.Secret)
			nsSecName := strings.Replace(secretName, "/", "-", -1)

			dh, ok := secret.Data["dhparam.pem"]
			if ok {
				pemFileName, err := ssl.AddOrUpdateDHParam(nsSecName, dh)
				if err != nil {
					glog.Warningf("unexpected error adding or updating dhparam %v file: %v", nsSecName, err)
				} else {
					sslDHParam = pemFileName
				}
			}
		}
	}

	cfg.SSLDHParam = sslDHParam

	// disable features are not available in some platforms
	switch runtime.GOARCH {
	case "arm", "arm64", "ppc64le":
		cfg.EnableModsecurity = false
	case "s390x":
		cfg.EnableModsecurity = false
		cfg.EnableBrotli = false
	}

	tc := ngx_config.TemplateConfig{
		ProxySetHeaders:         setHeaders,
		AddHeaders:              addHeaders,
		MaxOpenFiles:            maxOpenFiles,
		BacklogSize:             sysctlSomaxconn(),
		Backends:                ingressCfg.Backends,
		PassthroughBackends:     ingressCfg.PassthroughBackends,
		Servers:                 ingressCfg.Servers,
		TCPBackends:             ingressCfg.TCPEndpoints,
		UDPBackends:             ingressCfg.UDPEndpoints,
		HealthzURI:              ngxHealthPath,
		CustomErrors:            len(cfg.CustomHTTPErrors) > 0,
		Cfg:                     cfg,
		IsIPV6Enabled:           n.isIPV6Enabled && !cfg.DisableIpv6,
		RedirectServers:         redirectServers,
		IsSSLPassthroughEnabled: n.isSSLPassthroughEnabled,
		ListenPorts:             n.cfg.ListenPorts,
		PublishService:          n.GetPublishService(),
	}

	content, err := n.t.Write(tc)

	if err != nil {
		return err
	}

	err = n.testTemplate(content)
	if err != nil {
		return err
	}

	if glog.V(2) {
		src, _ := ioutil.ReadFile(cfgPath)
		if !bytes.Equal(src, content) {
			tmpfile, err := ioutil.TempFile("", "new-nginx-cfg")
			if err != nil {
				return err
			}
			defer tmpfile.Close()
			err = ioutil.WriteFile(tmpfile.Name(), content, 0644)
			if err != nil {
				return err
			}

			// executing diff can return exit code != 0
			diffOutput, _ := exec.Command("diff", "-u", cfgPath, tmpfile.Name()).CombinedOutput()

			glog.Infof("NGINX configuration diff\n")
			glog.Infof("%v\n", string(diffOutput))

			// Do not use defer to remove the temporal file.
			// This is helpful when there is an error in the
			// temporal configuration (we can manually inspect the file).
			// Only remove the file when no error occurred.
			os.Remove(tmpfile.Name())
		}
	}

	err = ioutil.WriteFile(cfgPath, content, 0644)
	if err != nil {
		return err
	}

	o, err := exec.Command(n.binary, "-s", "reload", "-c", cfgPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v\n%v", err, string(o))
	}

	return nil
}

// nginxHashBucketSize computes the correct nginx hash_bucket_size for a hash with the given longest key
func nginxHashBucketSize(longestString int) int {
	// See https://github.com/kubernetes/ingress-nginxs/issues/623 for an explanation
	wordSize := 8 // Assume 64 bit CPU
	n := longestString + 2
	aligned := (n + wordSize - 1) & ^(wordSize - 1)
	rawSize := wordSize + wordSize + aligned
	return nextPowerOf2(rawSize)
}

// http://graphics.stanford.edu/~seander/bithacks.html#RoundUpPowerOf2
// https://play.golang.org/p/TVSyCcdxUh
func nextPowerOf2(v int) int {
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v++

	return v
}
