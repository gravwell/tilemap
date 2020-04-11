package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"strconv"
)

const (
	TilesExtension string = `.tiles`

	envBindAddr      string = `BIND_ADDRESS`
	envBindPort      string = `BIND_PORT`
	envFileDir       string = `FILE_DIR`
	envTilesDir      string = `TILES_DIR`
	envLogFile       string = `LOG_FILE`
	envAccessLogFile string = `ACCESS_LOG_FILE`
)

type Config struct {
	BindAddr      string `json:"bind-addr"`
	BindPort      uint16 `json:"bind-port"`
	FileDir       string `json:"file-dir"`
	TilesDir      string `json:"tiles-dir"`
	AccessLogFile string `json:"access-log-file"`
	LogFile       string `json:"log-file"`
}

func LoadConfig(pth string) (c Config, err error) {
	var fin *os.File
	if fin, err = os.Open(pth); err != nil {
		return
	}
	if err = json.NewDecoder(fin).Decode(&c); err != nil {
		fin.Close()
	} else if err = fin.Close(); err == nil {
		err = c.validate()
	}
	return
}

func (c *Config) validate() (err error) {
	//load up any environment variables
	loadEnvString(&c.BindAddr, envBindAddr)
	loadEnvString(&c.FileDir, envFileDir)
	loadEnvString(&c.TilesDir, envTilesDir)
	loadEnvString(&c.LogFile, envLogFile)
	loadEnvString(&c.AccessLogFile, envAccessLogFile)
	loadEnvUint16(&c.BindPort, envBindPort)

	//check some sanity
	if c.BindAddr == `` {
		c.BindAddr = `0.0.0.0` //set to default bind all
	} else if net.ParseIP(c.BindAddr) == nil {
		err = fmt.Errorf("invalid bind address, could not parse %s as an IP", c.BindAddr)
		return
	}
	if c.BindPort == 0 || c.BindPort > 0xffff {
		err = fmt.Errorf("invalid bind port, must be between 0 and 65535")
		return
	}

	var fi os.FileInfo
	if fi, err = os.Stat(c.TilesDir); err != nil {
		return
	} else if fi.Mode().IsDir() == false {
		err = fmt.Errorf("map file path %s is not a directory", c.TilesDir)
		return
	}

	//check if we have a file directory specified
	if c.FileDir != `` {
		if fi, err = os.Stat(c.FileDir); err != nil {
			return
		} else if fi.Mode().IsDir() == false {
			err = fmt.Errorf("file directory path %s is not a directory", c.FileDir)
			return
		}
	}
	return
}

// validate should have been called when the config is loaded
func (c *Config) BindString() string {
	return fmt.Sprintf("%s:%d", c.BindAddr, c.BindPort)
}

func (c *Config) LogWriter() (wtr io.WriteCloser, err error) {
	return c.logWriter(c.LogFile)
}

func (c *Config) AccessLogWriter() (wtr io.WriteCloser, err error) {
	return c.logWriter(c.AccessLogFile)
}

func (c *Config) logWriter(v string) (wtr io.WriteCloser, err error) {
	if v == `` {
		wtr = &discarder{Writer: ioutil.Discard}
	} else {
		var f *os.File
		if f, err = os.OpenFile(v, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640); err != nil {
			return
		}
		wtr = f
	}
	return
}

type discarder struct {
	io.Writer
}

func (d *discarder) Close() error {
	return nil
}

func loadEnvString(v *string, key string) {
	if v == nil || *v != `` {
		return
	} else if x, ok := os.LookupEnv(key); ok {
		*v = x
	}
}

func loadEnvUint16(v *uint16, key string) {
	if v == nil || *v != 0 {
		return
	} else if x, ok := os.LookupEnv(key); ok {
		if val, err := strconv.Atoi(x); err == nil && val >= 0 && val <= 0xffff {
			*v = uint16(val)
		}
	}
}
