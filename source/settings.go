package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultWebAddr = ":24721"

type Settings struct {
	WebHosts []string `json:"web_hosts"`
	WebAddr  string   `json:"web_addr"`
	Radio    bool     `json:"radio"`
	Shuffle  bool     `json:"shuffle"`
}

// storedSettings also accepts the Alice-only keys an ytm-alice settings.json
// still carries; they are dropped on the next save.
type storedSettings struct {
	Settings
	AnnounceText   json.RawMessage `json:"announce_text"`
	AnnounceErrors json.RawMessage `json:"announce_errors"`
	PlaybackLED    json.RawMessage `json:"playback_led"`
	YouTubeOnly    json.RawMessage `json:"youtube_only"`
	VoiceCommands  json.RawMessage `json:"voice_commands"`
}

type SettingsPatch struct {
	WebHosts []string `json:"web_hosts,omitempty"`
	WebAddr  *string  `json:"web_addr,omitempty"`
	Radio    *bool    `json:"radio,omitempty"`
	Shuffle  *bool    `json:"shuffle,omitempty"`
}

func defaultSettings() Settings {
	return Settings{WebAddr: defaultWebAddr, Radio: true}
}

func settingsPath() string { return filepath.Join(dataDir(), "settings.json") }

func (s *Server) settingsLocked() Settings {
	value := defaultSettings()
	if s.settings != nil {
		value = *s.settings
	}
	value.WebHosts = append([]string(nil), value.WebHosts...)
	return value
}

func (s *Server) getSettings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settingsLocked()
}

func validateSettings(value Settings) error {
	if len(value.WebHosts) > 16 {
		return errors.New("можно разрешить до 16 доменов панели")
	}
	for _, host := range value.WebHosts {
		if !validWebHost(host) {
			return errors.New("web_hosts: нужны точные имена доменов без схемы, порта и пути")
		}
	}

	if value.WebAddr != "off" {
		host, port, err := net.SplitHostPort(value.WebAddr)
		n, numErr := strconv.Atoi(port)
		if err != nil || numErr != nil || n < 1 || n > 65535 || (host != "" && host != "localhost" && net.ParseIP(host) == nil) {
			return errors.New("адрес панели: :24721, IP:порт или off")
		}
		if n == 8008 || n == 18883 {
			return errors.New("порт занят службой трансляции или аудиоплеером")
		}
	}
	return nil
}

func writeSettings(value Settings) error {
	if err := os.MkdirAll(dataDir(), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dataDir(), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), settingsPath())
	}
	return err
}

func (s *Server) loadSettings() error {
	value := defaultSettings()
	f, err := os.Open(settingsPath())
	if os.IsNotExist(err) {
		// Preserve bookmarked URLs when upgrading an existing installation.
		if _, err := os.Stat(filepath.Join(dataDir(), "state.json")); err == nil {
			value.WebAddr = ":80"
		}
		if err := writeSettings(value); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		defer f.Close()
		decoder := json.NewDecoder(io.LimitReader(f, 32<<10))
		decoder.DisallowUnknownFields()
		stored := storedSettings{Settings: value}
		if decoder.Decode(&stored) != nil || decoder.Decode(new(any)) != io.EOF {
			return errors.New("повреждён settings.json")
		}
		value = stored.Settings
	}
	if err := validateSettings(value); err != nil {
		return err
	}
	s.settings = &value
	return nil
}

func (s *Server) settingsCommand(r request) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.settingsLocked()
	if r.Action == "config-set" {
		p := r.Settings
		if p == nil {
			return response{Error: "нужны настройки"}
		}
		if p.WebHosts != nil {
			value.WebHosts = append([]string{}, p.WebHosts...)
		}
		if p.WebAddr != nil {
			value.WebAddr = *p.WebAddr
		}
		if p.Radio != nil {
			value.Radio = *p.Radio
		}
		if p.Shuffle != nil {
			value.Shuffle = *p.Shuffle
		}
		if err := validateSettings(value); err != nil {
			return response{Error: err.Error()}
		}
		if err := writeSettings(value); err != nil {
			return response{Error: "не удалось сохранить настройки"}
		}
		s.settings = &value
	}
	address := s.webAddr
	if address == "" {
		address = effectiveWebAddr(value)
	}
	return response{Settings: &value, WebAddr: address}
}

func effectiveWebAddr(settings Settings) string {
	if value := os.Getenv("YTM_ALICE_WEB_ADDR"); value != "" {
		return value
	}
	return settings.WebAddr
}

func configRequest(args []string) (request, error) {
	if len(args) == 0 || (len(args) == 1 && args[0] == "get") {
		return request{Action: "config-get"}, nil
	}
	if len(args) < 3 || args[0] != "set" {
		return request{}, errors.New("config get | config set КЛЮЧ ЗНАЧЕНИЕ")
	}
	key, value := args[1], strings.Join(args[2:], " ")
	var raw any = value
	switch key {
	case "radio", "shuffle":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return request{}, errors.New("нужно true или false")
		}
		raw = v
	case "web_hosts":
		var hosts []string
		if json.Unmarshal([]byte(value), &hosts) != nil || hosts == nil {
			return request{}, errors.New("нужен JSON-массив доменов")
		}
		raw = hosts
	case "web_addr":
	default:
		return request{}, fmt.Errorf("неизвестная настройка %s", key)
	}
	data, _ := json.Marshal(map[string]any{key: raw})
	var patch SettingsPatch
	if err := json.Unmarshal(data, &patch); err != nil {
		return request{}, err
	}
	return request{Action: "config-set", Settings: &patch}, nil
}
