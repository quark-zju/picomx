// Package config defines trusted, compile-time local SMTP policy hooks.
package config

import (
	"io"
	"net/mail"
	"net/netip"
	"time"
)

// Address is a normalized SMTP envelope address.
type Address struct {
	Local  string
	Domain string
}

func (a Address) String() string {
	if a.Local == "" && a.Domain == "" {
		return ""
	}
	return a.Local + "@" + a.Domain
}

// ConnectionInfo contains bounded, normalized peer metadata.
type ConnectionInfo struct {
	RemoteIP netip.Addr
	HELO     string
	TLS      bool
}

type RecipientRequest struct {
	Connection   ConnectionInfo
	EnvelopeFrom Address
	Recipient    Address
}

type RecipientAction uint8

const (
	RecipientRejectPolicy RecipientAction = iota
	RecipientRejectUnknown
	RecipientTempFail
	RecipientAccept
)

type RecipientDecision struct {
	Action RecipientAction
	Reason string
}

type MessageMetadata struct {
	Connection   ConnectionInfo
	EnvelopeFrom Address
	Recipients   []Address
	ReceivedAt   time.Time
	Octets       uint64
}

type MessageRequest interface {
	Metadata() MessageMetadata
	Header() mail.Header
	Open() (io.ReadCloser, error)
}

type MessageAction uint8

const (
	MessageTempFail MessageAction = iota
	MessageRejectPolicy
	MessageAccept
)

type MessageDecision struct {
	Action MessageAction
	Reason string
}

type SMTPPolicy interface {
	EvaluateRecipient(RecipientRequest) RecipientDecision
	EvaluateMessage(MessageRequest) MessageDecision
}

var smtpPolicyFactory func() SMTPPolicy

// SetSMTPPolicyFactory installs the trusted local compile-time policy.
func SetSMTPPolicyFactory(factory func() SMTPPolicy) {
	smtpPolicyFactory = factory
}

// NewSMTPPolicy returns the local policy or the compatibility default.
func NewSMTPPolicy() SMTPPolicy {
	if smtpPolicyFactory != nil {
		return smtpPolicyFactory()
	}
	return defaultSMTPPolicy{}
}

type defaultSMTPPolicy struct{}

func (defaultSMTPPolicy) EvaluateRecipient(RecipientRequest) RecipientDecision {
	return RecipientDecision{Action: RecipientAccept, Reason: "configured_domain"}
}

func (defaultSMTPPolicy) EvaluateMessage(MessageRequest) MessageDecision {
	return MessageDecision{Action: MessageAccept, Reason: "default_accept"}
}
