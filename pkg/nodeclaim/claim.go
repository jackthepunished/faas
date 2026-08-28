// Package nodeclaim defines the provider-neutral handoff between machine
// creation and Gregale compute-node adoption.
//
// A claim contains only connection and storage facts about an already-created
// machine. Release, PKI, daemon topology, and database settings remain owned
// by the signed production manifest and release artifact.
package nodeclaim

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion = "gregale.dev/v1alpha1"
	Kind       = "ComputeNodeClaim"
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`)
	userPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
)

// Claim is the YAML/JSON representation of one already-created compute
// machine. The shape is deliberately small so provider adapters can emit it
// without knowing Gregale's internal manifest or Ansible schema.
type Claim struct {
	APIVersion string   `yaml:"api_version" json:"api_version"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

type Metadata struct {
	Name string `yaml:"name" json:"name"`
}

type Spec struct {
	SSH     SSH     `yaml:"ssh" json:"ssh"`
	Storage Storage `yaml:"storage,omitempty" json:"storage,omitempty"`
}

type SSH struct {
	Host string `yaml:"host" json:"host"`
	User string `yaml:"user,omitempty" json:"user,omitempty"`
	Port int    `yaml:"port,omitempty" json:"port,omitempty"`
}

type Storage struct {
	Device string `yaml:"device,omitempty" json:"device,omitempty"`
	Format bool   `yaml:"format,omitempty" json:"format,omitempty"`
}

// Node is the normalized connection/storage input consumed by the join
// coordinator. Defaults are applied by Normalize after validation.
type Node struct {
	Name          string
	SSHHost       string
	SSHUser       string
	SSHPort       int
	StorageDevice string
	FormatStorage bool
}

// Error is one deterministic claim validation failure.
type Error struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e Error) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Message) }

type Errors []Error

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

func (e Errors) Is(target error) bool { return target == ErrInvalid }

var ErrInvalid = errors.New("nodeclaim: invalid")

func Load(path string) (*Claim, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("nodeclaim: read %s: %w", path, err)
	}
	return Parse(body)
}

func Parse(body []byte) (*Claim, error) {
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	var claim Claim
	if err := dec.Decode(&claim); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("nodeclaim: empty file")
		}
		return nil, fmt.Errorf("nodeclaim: parse: %w", err)
	}
	return &claim, nil
}

// Validate checks the claim without contacting the provider or target host.
// Host-key pinning is deliberately outside this first claim contract: the
// deployment runner still relies on its existing known_hosts trust bootstrap.
func (c Claim) Validate() Errors {
	var errs Errors
	if c.APIVersion != APIVersion {
		errs = append(errs, Error{"api_version", fmt.Sprintf("must be %q", APIVersion)})
	}
	if c.Kind != Kind {
		errs = append(errs, Error{"kind", fmt.Sprintf("must be %q", Kind)})
	}
	if c.Metadata.Name == "" {
		errs = append(errs, Error{"metadata.name", "is required"})
	} else if !namePattern.MatchString(c.Metadata.Name) {
		errs = append(errs, Error{"metadata.name", "must contain only lowercase letters, digits, dots, or dashes and start with a lowercase letter or digit"})
	}
	if c.Spec.SSH.Host == "" {
		errs = append(errs, Error{"spec.ssh.host", "is required"})
	} else if err := validateHost(c.Spec.SSH.Host); err != nil {
		errs = append(errs, Error{"spec.ssh.host", err.Error()})
	}
	if c.Spec.SSH.User != "" && !userPattern.MatchString(c.Spec.SSH.User) {
		errs = append(errs, Error{"spec.ssh.user", "must be a simple SSH account name"})
	}
	if c.Spec.SSH.Port < 0 || c.Spec.SSH.Port > 65535 {
		errs = append(errs, Error{"spec.ssh.port", "must be between 1 and 65535 when supplied"})
	} else if c.Spec.SSH.Port == 0 {
		// zero means the documented default of 22
	} else if c.Spec.SSH.Port < 1 {
		errs = append(errs, Error{"spec.ssh.port", "must be between 1 and 65535"})
	}
	if c.Spec.Storage.Device != "" {
		if !strings.HasPrefix(c.Spec.Storage.Device, "/") || strings.IndexFunc(c.Spec.Storage.Device, unicode.IsSpace) >= 0 {
			errs = append(errs, Error{"spec.storage.device", "must be an absolute stable device path without whitespace"})
		}
	}
	if c.Spec.Storage.Format && c.Spec.Storage.Device == "" {
		errs = append(errs, Error{"spec.storage.format", "requires spec.storage.device"})
	}
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return errs
}

func (c Claim) Normalize() Node {
	user := c.Spec.SSH.User
	if user == "" {
		user = "root"
	}
	port := c.Spec.SSH.Port
	if port == 0 {
		port = 22
	}
	return Node{
		Name:          c.Metadata.Name,
		SSHHost:       c.Spec.SSH.Host,
		SSHUser:       user,
		SSHPort:       port,
		StorageDevice: c.Spec.Storage.Device,
		FormatStorage: c.Spec.Storage.Format,
	}
}

func validateHost(host string) error {
	if strings.TrimSpace(host) != host || strings.Contains(host, "/") || strings.Contains(host, "://") || strings.IndexFunc(host, unicode.IsSpace) >= 0 {
		return errors.New("must be an IP address or DNS hostname, without a scheme or path")
	}
	if ip := net.ParseIP(host); ip != nil {
		return nil
	}
	if !hostPattern.MatchString(host) || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return errors.New("must be an IP address or DNS hostname")
	}
	return nil
}
