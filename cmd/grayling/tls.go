package main

import (
	"crypto/tls"
	"errors"
)

// listenerTLS validates certificate material before the App opens any stores.
func listenerTLS(agentEnabled bool, certificateFile, keyFile string) (*tls.Config, error) {
	if certificateFile == "" && keyFile == "" {
		if agentEnabled {
			return nil, errors.New("Agent requires -tls-cert and -tls-key on this listener")
		}
		return nil, nil
	}
	if certificateFile == "" || keyFile == "" {
		return nil, errors.New("-tls-cert and -tls-key must be provided together")
	}
	certificate, err := tls.LoadX509KeyPair(certificateFile, keyFile)
	if err != nil {
		return nil, errors.New("unable to load listener TLS certificate and key")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}, nil
}
