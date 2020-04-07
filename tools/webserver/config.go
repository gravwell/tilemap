package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
)

const (
	TilesExtension string = `.tiles`
)

type Config struct {
	BindAddr string `json:"bind-addr"`
	BindPort uint   `json:"bind-port"`
	FileDir  string `json:"file-dir"`
	MapDir   string `json:"map-dir"`
	LogFile  string `json:"log-file"`
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
	if fi, err = os.Stat(c.MapDir); err != nil {
		return
	} else if fi.Mode().IsDir() == false {
		err = fmt.Errorf("map file path %s is not a directory", c.MapDir)
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
	if c.LogFile == `` {
		wtr = &discarder{Writer: ioutil.Discard}
	} else {
		var f *os.File
		if f, err = os.OpenFile(c.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0640); err != nil {
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
