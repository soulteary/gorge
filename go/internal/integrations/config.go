// Package integrations owns opt-in external transports and durable inbound intake.
package integrations

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
)

type Target struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	User        string `json:"user,omitempty"`
	Secret      string `json:"secret,omitempty"`
	From        string `json:"from,omitempty"`
	Region      string `json:"region,omitempty"`
	ConsumerKey string `json:"consumerKey,omitempty"`
	PrivateKey  string `json:"privateKey,omitempty"`
}
type Config struct {
	Listen             string            `json:"listen"`
	Token              string            `json:"token"`
	DSN                string            `json:"dsn"`
	ConduitURI         string            `json:"conduitURI"`
	ConduitToken       string            `json:"conduitToken"`
	Inbound            []string          `json:"inbound"`
	FactDSN            string            `json:"factDSN"`
	Targets            map[string]Target `json:"targets"`
	RelayWorkers       int               `json:"relayWorkers"`
	InboxRetentionDays int               `json:"inboxRetentionDays"`
}

func Load(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(raw, &c); err != nil {
		return c, errors.New("invalid integrations configuration")
	}
	if c.Listen == "" {
		c.Listen = ":8210"
	}
	if c.RelayWorkers == 0 {
		c.RelayWorkers = 4
	}
	if c.InboxRetentionDays == 0 {
		c.InboxRetentionDays = 30
	}
	return c, c.Validate()
}
func (c Config) Validate() error {
	if c.RelayWorkers < 0 || c.RelayWorkers > 16 {
		return errors.New("relayWorkers must be between 1 and 16")
	}
	if c.InboxRetentionDays < 0 || c.InboxRetentionDays > 3650 {
		return errors.New("invalid inbox retention")
	}
	if c.Token == "" || c.DSN == "" {
		return errors.New("integrations require service token and durable MySQL DSN")
	}
	if len(c.Inbound) > 0 || c.FactDSN != "" {
		if c.ConduitURI == "" || c.ConduitToken == "" {
			return errors.New("inbound/fact require authenticated Conduit")
		}
	}
	if c.ConduitURI != "" {
		u, e := url.Parse(c.ConduitURI)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid Conduit endpoint")
		}
	}
	for _, v := range c.Inbound {
		if v != "mailgun" && v != "postmark" && v != "sendgrid" && v != "raw" {
			return errors.New("unsupported inbound provider")
		}
	}
	for k, t := range c.Targets {
		if k == "" || len(k) > 128 {
			return errors.New("invalid target key")
		}
		switch t.Type {
		case "twilio":
			if t.User == "" || t.Secret == "" || t.From == "" {
				return errors.New("incomplete Twilio credentials")
			}
		case "sns":
			if t.User == "" || t.Secret == "" || t.Region == "" {
				return errors.New("incomplete SNS credentials")
			}
		case "asana", "github":
		case "jira":
			if t.ConsumerKey == "" || t.PrivateKey == "" {
				return errors.New("incomplete JIRA OAuth1 credentials")
			}

			if _, e := oauthHeader(t, "startup-validation", "POST", t.URL); e != nil {
				return errors.New("invalid JIRA RSA signing key")
			}
		default:
			return errors.New("unsupported integration target")
		}
		u, e := url.Parse(t.URL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Path, "..") {
			return errors.New("target requires fixed HTTPS base URL")
		}
		if t.Type == "twilio" && (u.Host != "api.twilio.com" || strings.TrimRight(u.Path, "/") != "/2010-04-01") {
			return errors.New("twilio base URL must be canonical")
		}
		if t.Type == "sns" && (u.Host != "sns."+t.Region+".amazonaws.com" || u.Path != "") {
			return errors.New("SNS endpoint must match region")
		}
		if t.Type == "github" && (u.Host != "api.github.com" || strings.TrimRight(u.Path, "/") != "") {
			return errors.New("GitHub base URL must be canonical")
		}
		if t.Type == "asana" && (u.Host != "app.asana.com" || strings.TrimRight(u.Path, "/") != "/api/1.0") {
			return errors.New("asana base URL must be canonical")
		}
	}
	return nil
}
