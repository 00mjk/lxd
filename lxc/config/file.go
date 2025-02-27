package config

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v2"

	"github.com/lxc/lxd/shared"
)

// LoadConfig reads the configuration from the config path; if the path does
// not exist, it returns a default configuration.
func LoadConfig(path string) (*Config, error) {
	// Open the config file
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("Unable to read the configuration file: %w", err)
	}

	// Decode the YAML document
	c := NewConfig(filepath.Dir(path), false)
	err = yaml.Unmarshal(content, &c)
	if err != nil {
		return nil, fmt.Errorf("Unable to decode the configuration: %w", err)
	}

	for k, r := range c.Remotes {
		if !r.Public && r.AuthType == "" {
			r.AuthType = "tls"
			c.Remotes[k] = r
		}
	}

	// Apply the global (system-wide) remotes
	globalConf := NewConfig("", false)
	content, err = ioutil.ReadFile(globalConf.GlobalConfigPath("config.yml"))
	if err == nil {
		err = yaml.Unmarshal(content, &globalConf)
		if err != nil {
			return nil, fmt.Errorf("Unable to decode the configuration: %w", err)
		}
		for k, r := range globalConf.Remotes {
			if _, ok := c.Remotes[k]; !ok {
				r.Global = true
				c.Remotes[k] = r
			}
		}
	}

	// Set default values
	if c.Remotes == nil {
		c.Remotes = make(map[string]Remote)
	}

	// Apply the static remotes
	for k, v := range StaticRemotes {
		if c.Remotes[k].Project != "" {
			v.Project = c.Remotes[k].Project
		}

		c.Remotes[k] = v
	}

	// If the environment specifies a remote this takes priority over what
	// is defined in the configuration
	envDefaultRemote := os.Getenv("LXC_REMOTE")
	if len(envDefaultRemote) > 0 {
		c.DefaultRemote = envDefaultRemote
	} else if c.DefaultRemote == "" {
		c.DefaultRemote = DefaultConfig.DefaultRemote
	}

	// NOTE: Remove this once we only see a small fraction of non-simplestreams users
	// Upgrade users to the "simplestreams" protocol
	images, ok := c.Remotes["images"]
	if ok && images.Protocol != ImagesRemote.Protocol && images.Addr == ImagesRemote.Addr {
		c.Remotes["images"] = ImagesRemote
		err = c.SaveConfig(path)
		if err != nil {
			return nil, err
		}
	}

	return c, nil
}

// SaveConfig writes the provided configuration to the config file.
func (c *Config) SaveConfig(path string) error {
	// Create a new copy for the config file
	conf := Config{}
	err := shared.DeepCopy(c, &conf)
	if err != nil {
		return fmt.Errorf("Unable to copy the configuration: %w", err)
	}

	// Remove the global remotes
	for k, v := range c.Remotes {
		if v.Global == true {
			delete(conf.Remotes, k)
		}
	}

	// Remove the static remotes
	for k := range StaticRemotes {
		if k == DefaultConfig.DefaultRemote {
			continue
		}

		delete(conf.Remotes, k)
	}

	// Create the config file (or truncate an existing one)
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("Unable to create the configuration file: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Write the new config
	data, err := yaml.Marshal(conf)
	if err != nil {
		return fmt.Errorf("Unable to marshal the configuration: %w", err)
	}

	_, err = f.Write(data)
	if err != nil {
		return fmt.Errorf("Unable to write the configuration: %w", err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("Unable to close the configuration file: %w", err)
	}

	return nil
}
