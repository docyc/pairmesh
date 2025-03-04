//go:build windows || darwin
// +build windows darwin

// Copyright 2021 PairMesh, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package entry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/pairmesh/pairmesh/internal/messagebox"
	"go.uber.org/atomic"

	"github.com/emersion/go-autostart"
	"github.com/pairmesh/pairmesh/i18n"
	"github.com/pairmesh/pairmesh/node/api"
	"github.com/pairmesh/pairmesh/node/config"
	"github.com/pairmesh/pairmesh/node/device"
	"github.com/pairmesh/pairmesh/node/driver"
	"github.com/pairmesh/pairmesh/pkg/logutil"
	"github.com/pairmesh/pairmesh/version"
	"github.com/skratchdot/open-golang/open"
	"go.uber.org/zap"
)

type (
	baseApp struct {
		nodeInfo LoginNodeInfo
		cfg      *config.Config
		dev      device.Device
		api      *api.Client
		driver   driver.Driver
		auto     *autostart.App
		events   chan struct{}

		cancel context.CancelFunc

		initialized atomic.Bool
	}

	LoginNodeInfo struct {
		Port    int    `json:"port"`
		Machine string `json:"machine"`
	}
)

func precheck() error {
	// Normalize the gateway scheme
	gateway := config.APIGateway()
	u, err := url.Parse(gateway)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unknown gateway scheme %s", u.Scheme)
	}
	if u.Path != "" {
		return fmt.Errorf("incorrect gateway address %s", gateway)
	}
	return nil
}

// Run startups the GUI desktop application
func Run() {
	logutil.InitLogger()

	if err := precheck(); err != nil {
		messagebox.Fatal("Precheck failed", err.Error())
	}

	cfg := &config.Config{}
	err := cfg.Load()
	if err != nil {
		messagebox.Fatal("Load configuration failed", err.Error())
	}

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		messagebox.Fatal("Listen HTTP failed", err.Error())
	}

	app := newOSApp()
	app.nodeInfo = LoginNodeInfo{
		Port:    listener.Addr().(*net.TCPAddr).Port,
		Machine: cfg.MachineID,
	}
	app.cfg = cfg
	app.api = api.New(config.APIGateway(), cfg.Token, cfg.MachineID)

	if err := exchangeAuthKeyIfNeed(app.api, app.cfg); err != nil {
		messagebox.Fatal("Exchange auth key failed", err.Error())
	}

	// Start the daemon HTTP server
	go func() {
		http.Handle("/local/auth/callback", http.HandlerFunc(app.onLoginCallback))
		err := http.Serve(listener, nil)
		if err != nil {
			messagebox.Fatal("Serve listener failed", err.Error())
		}
	}()

	zap.L().Info("Startup node successfully")

	if err := app.Run(); err != nil {
		messagebox.Fatal("Run application failed", err.Error())
	}
}

func (app *osApp) Run() error {
	dev, err := device.NewDevice()
	if err != nil {
		return fmt.Errorf("preflight device failed. error message: %s", err.Error())
	}
	app.dev = dev

	d := driver.New(app.cfg, dev, app.api)

	ctx, cancel := context.WithCancel(context.Background())

	// Preflight the driver if token existing.
	if !app.cfg.IsGuest() {
		zap.L().Info("Token found and will do preflight")
		err = d.Preflight()
		if err != nil {
			zap.L().Error("Preflight driver failed", zap.Error(err))
			// TODO: check if are recoverable errors
			app.cfg.Token = ""
			_ = app.cfg.Save()
		} else {
			zap.L().Info("Preflight success and will serve the application")
			go d.Drive(ctx)
		}
	}
	app.cancel = cancel
	app.driver = d

	zap.L().Info("Driver initialized successfully")

	app.auto = &autostart.App{
		Name:        "PairMesh",
		DisplayName: "PairMesh Client",
		Exec:        os.Args,
	}

	// Refresh UI dynamically.
	go app.refreshTray()

	app.run()

	return nil
}

func (app *osApp) refreshEvent() {
	app.events <- struct{}{}
}

func (app *osApp) refreshTray() {
	var cachedSummary *driver.Summary

	timer := time.After(0)

	refresh := func() {
		timer = time.After(3 * time.Second)
		if app.cfg.IsGuest() {
			return
		}
		if !app.initialized.Load() {
			return
		}

		summary := app.driver.Summarize()
		isEqual := cachedSummary != nil && cachedSummary.Equal(summary)
		if isEqual {
			return
		}

		cachedSummary = summary
		app.render(summary)

		// Workaround for MacOS calculate menu item tabstop location
		// see: ./systray/system_darwin.m:246
		if runtime.GOOS == "darwin" {
			needReRender := false
			for i := range summary.Mesh.MyDevices {
				if i == 0 {
					continue
				}
				if len(summary.Mesh.MyDevices[i].Name) > len(summary.Mesh.MyDevices[i-1].Name) {
					needReRender = true
					break
				}
			}
			if needReRender {
				app.render(summary)
			}
		}
	}

	for {
		select {
		case <-app.events:
			refresh()
		case <-timer:
			refresh()
		}
	}
}

func (app *osApp) onQuit() {
	zap.L().Info("Application ready to quit")

	if app.cancel != nil {
		zap.L().Info("Cancel all running operations")
		app.cancel()
	}

	err := app.dev.Close()
	if err != nil {
		zap.L().Error("Close device failed", zap.Error(err))
	}

	zap.L().Info("Device closed")

	err = app.dev.Down()
	if err != nil {
		zap.L().Error("Down device failed", zap.Error(err))
	}

	zap.L().Info("Device down")

	app.driver.Terminate()
	app.dispose()
}

func (app *osApp) onOpenConsole() {
	_ = open.Run(config.MyGateway())
}

func (app *osApp) onOpenLoginWeb() {
	data, _ := json.Marshal(app.nodeInfo)
	info := base64.RawStdEncoding.EncodeToString(data)
	addr := fmt.Sprintf("%s/login?client=%s", config.MyGateway(), info)
	_ = open.Run(addr)
}

func (app *osApp) onAutoStart() {
	var switcher func() error
	if app.auto.IsEnabled() {
		switcher = app.auto.Disable
	} else {
		switcher = app.auto.Enable
	}
	err := switcher()
	if err != nil {
		zap.L().Error("Switch application auto start failed", zap.Error(err))
		return
	}
	app.refreshEvent()

	zap.L().Info("Switch application auto start when bootstrap", zap.Bool("enabled", app.auto.IsEnabled()))
}

func (app *osApp) switchDriverEnable(currentEnabled bool) {
	if currentEnabled {
		app.driver.Disable()
	} else {
		app.driver.Enable()
	}
	app.refreshEvent()
}

func (app *osApp) onOpenAbout() {
	app.showMessage(i18n.L("message.about"), i18n.L("message.version", version.NewVersion().FullInfo()))
}

func (app *osApp) onLogout() {
	zap.L().Info("User logout...")

	cloned := app.cfg.Clone()
	cloned.Token = ""
	err := cloned.Save()
	if err != nil {
		zap.L().Error("Save changed configuration failed", zap.Error(err))
		return
	}
	// Replace the current node token.
	app.cfg.Token = ""

	app.cancel()
	app.driver.Terminate()
	zap.L().Info("Driver terminated...")

	app.driver = driver.New(app.cfg, app.dev, app.api)
	app.refreshEvent()
}

func (app *osApp) onLoginCallback(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	zap.L().Info("Receive login callback", zap.String("token", token))

	err := app.cfg.SetJWTToken(token)
	if err != nil {
		zap.L().Error("Save access token failed", zap.Error(err))
	}

	app.api.SetToken(app.cfg.Token)
	err = app.driver.Preflight()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	app.cancel = cancel

	go app.driver.Drive(ctx)

	app.refreshEvent()

	http.Redirect(w, r, fmt.Sprintf("%s/login/auth/success", config.MyGateway()), http.StatusPermanentRedirect)
}
