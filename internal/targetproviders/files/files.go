// SPDX-FileCopyrightText: 2024 Paulo Almeida <almeidapaulopt@gmail.com>
// SPDX-License-Identifier: MIT
package files

import (
	"context"
	"fmt"
	"net/url"
	"sync"

	"github.com/almeidapaulopt/tsdproxy/internal/config"
	"github.com/almeidapaulopt/tsdproxy/internal/proxyconfig"
	"github.com/almeidapaulopt/tsdproxy/internal/targetproviders"

	"github.com/creasty/defaults"
	"github.com/fsnotify/fsnotify"
	"github.com/rs/zerolog"
	"github.com/spf13/viper"
)

type (
	// Client struct implements TargetProvider
	Client struct {
		proxies  map[string]ProxyConfig
		log      zerolog.Logger
		viper    *viper.Viper
		config   FileConfig
		name     string
		filename string

		eventsChan chan targetproviders.TargetEvent
		errChan    chan error

		mutex sync.Mutex
	}

	// FileConfig struct stores File target provider configuration.
	FileConfig struct {
		Proxies               map[string]ProxyConfig `validate:"dive"`
		DefaultProxyProvider  string                 `validate:"required"`
		DefaultProxyAccessLog bool                   `default:"true" validate:"boolean"`
	}

	ProxyConfig struct {
		URL           string `validate:"required,uri"`
		ProxyProvider string
		Tailscale     proxyconfig.Tailscale
	}
)

// New function returns a new Files TargetProvider
func New(log zerolog.Logger, name string, file string) (*Client, error) {
	newlog := log.With().Str("file", name).Logger()

	target := &FileConfig{}

	v, err := config.NewViper(file, target)
	if err != nil {
		return nil, err
	}

	// load default values
	err = defaults.Set(target)
	if err != nil {
		fmt.Printf("Error loading defaults: %v", err)
	}

	c := &Client{
		log:        newlog,
		name:       name,
		filename:   file,
		viper:      v,
		config:     *target,
		proxies:    make(map[string]ProxyConfig),
		eventsChan: make(chan targetproviders.TargetEvent),
		errChan:    make(chan error),
	}

	return c, nil
}

func (c *Client) GetAllProxies() (map[string]*proxyconfig.Config, error) {
	var wg sync.WaitGroup
	proxies := map[string]*proxyconfig.Config{}

	for name, proxyconfig := range c.config.Proxies {
		// create the proxy configs in parallel.
		wg.Add(1)

		go func() {
			defer wg.Done()

			pcfg, err := c.newProxyConfig(name, proxyconfig)
			if err != nil {
				c.log.Error().Err(err).Msg("error initializing proxy")
				return
			}
			proxies[name] = pcfg
		}()
	}
	wg.Wait()

	return proxies, nil
}

// newProxyConfig method returns a new proxyconfig.Config
func (c *Client) newProxyConfig(name string, p ProxyConfig) (*proxyconfig.Config, error) {
	targetURL, err := url.Parse(p.URL)
	if err != nil {
		return nil, fmt.Errorf("error parsing target URL: %w", err)
	}

	proxyURL, err := url.Parse(name)
	if err != nil {
		return nil, fmt.Errorf("error parsing proxy URL: %w", err)
	}

	proxyProvider := c.config.DefaultProxyProvider
	if p.ProxyProvider != "" {
		proxyProvider = p.ProxyProvider
	}

	proxyAccessLog := proxyconfig.ProxyAccessLog

	pcfg := &proxyconfig.Config{
		TargetID:       name,
		TargetURL:      targetURL,
		ProxyURL:       proxyURL,
		Hostname:       name,
		TargetProvider: c.name,
		Tailscale:      &p.Tailscale,
		ProxyProvider:  proxyProvider,
		ProxyAccessLog: proxyAccessLog,
	}

	c.addTarget(p, name)

	return pcfg, nil
}

func (c *Client) WatchEvents(ctx context.Context, eventsChan chan targetproviders.TargetEvent, errChan chan error) {
	c.viper.WatchConfig()
	c.viper.OnConfigChange(c.onConfigChange)
	go func() {
		for {
			select {
			case filesEvent := <-c.eventsChan:
				eventsChan <- filesEvent

			case err := <-c.errChan:
				errChan <- err
			}
		}
	}()
}

func (c *Client) onConfigChange(e fsnotify.Event) {
	if !e.Op.Has(fsnotify.Write) {
		return
	}
	c.log.Info().Str("filename", e.Name).Msg("config changed, reloading")
	oldConfig := c.config
	if err := c.viper.Unmarshal(&c.config); err != nil {
		c.log.Error().Err(err).Msg("error loading config")
	}

	// delete proxies that don't exist in new config
	for name := range oldConfig.Proxies {
		if _, ok := c.config.Proxies[name]; !ok {
			c.eventsChan <- targetproviders.TargetEvent{
				ID:             name,
				TargetProvider: c,
				Action:         targetproviders.ActionStop,
			}
		}
	}

	for name := range c.config.Proxies {
		// start new proxies
		if _, ok := oldConfig.Proxies[name]; !ok {
			c.eventsChan <- targetproviders.TargetEvent{
				ID:             name,
				TargetProvider: c,
				Action:         targetproviders.ActionStart,
			}
			continue
		}

		// restart if the proxy configuration changed
		if oldConfig.Proxies[name] != c.config.Proxies[name] {
			c.eventsChan <- targetproviders.TargetEvent{
				ID:             name,
				TargetProvider: c,
				Action:         targetproviders.ActionRestart,
			}
		}
	}
}

func (c *Client) GetDefaultProxyProviderName() string {
	return c.config.DefaultProxyProvider
}

func (c *Client) Close() {
	for name := range c.proxies {
		c.eventsChan <- targetproviders.TargetEvent{
			ID:             name,
			TargetProvider: c,
			Action:         targetproviders.ActionStop,
		}
	}
}

func (c *Client) AddTarget(id string) (*proxyconfig.Config, error) {
	proxy, ok := c.config.Proxies[id]
	if !ok {
		return nil, fmt.Errorf("target %s not found", id)
	}

	pcfg, err := c.newProxyConfig(id, proxy)
	if err != nil {
		return nil, err
	}

	return pcfg, nil
}

func (c *Client) DeleteProxy(id string) error {
	if _, ok := c.proxies[id]; !ok {
		return fmt.Errorf("target %s not found", id)
	}
	c.deleteTarget(id)
	return nil
}

// addTarget method add a target the proxies map
func (c *Client) addTarget(cfg ProxyConfig, name string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.proxies[name] = cfg
}

// deleteTarget method deletes a target from the proxies map
func (c *Client) deleteTarget(name string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	delete(c.proxies, name)
}
