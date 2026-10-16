// Package node is the judge node's JudgeControl client.
//
// Callers see only Options, JudgeFunc, FromEnv, and Run. Dial, handshake,
// heartbeat, slot accounting, invalidation, and stream send serialization stay
// inside the package.
package node

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	protocolVersion  = "1"
	defaultMaxSlots  = 1
	defaultWorkDir   = "work"
	defaultHeartbeat = 10 * time.Second
	minReconnectWait = 200 * time.Millisecond
	maxReconnectWait = 15 * time.Second
)

// errHandshakeRejected is returned when the server refuses the node.
var errHandshakeRejected = errors.New("handshake rejected")

// Options configures a judge node process. Credential is never logged.
type Options struct {
	Addr       string
	NodeID     string
	Credential string
	WorkDir    string
	CAFile     string
	Insecure   bool
	MaxSlots   int32
	// Judge is the seam for compiling and scoring. Production cmd wires
	// attempt.Runner.Run; tests pass a stub. This package does not import
	// internal/attempt.
	Judge JudgeFunc
	// ExecutionProfileIDs is advertised in the handshake. An empty list
	// is an error; this package does not invent profile IDs.
	ExecutionProfileIDs []string

	// dialer is an internal seam for bufconn tests. Nil uses the default
	// gRPC resolver against Addr.
	dialer func(context.Context, string) (net.Conn, error)
}

// JudgeFunc runs one dispatched attempt. Return nil when the attempt was
// cancelled or invalidated; Run will not send an AttemptResult in that case.
type JudgeFunc func(ctx context.Context, d *judgev1.DispatchAttempt) *judgev1.AttemptResult

// FromEnv loads Options from EPHYRA_JUDGE_* environment variables.
// ADDR, NODE_ID, and CREDENTIAL are required. MaxSlots defaults to 1 and
// WorkDir defaults to "work". INSECURE=1 selects insecure gRPC credentials.
func FromEnv() (Options, error) {
	opts := Options{
		Addr:       strings.TrimSpace(os.Getenv("EPHYRA_JUDGE_ADDR")),
		NodeID:     strings.TrimSpace(os.Getenv("EPHYRA_JUDGE_NODE_ID")),
		Credential: os.Getenv("EPHYRA_JUDGE_CREDENTIAL"),
		WorkDir:    strings.TrimSpace(os.Getenv("EPHYRA_JUDGE_WORKDIR")),
		CAFile:     strings.TrimSpace(os.Getenv("EPHYRA_JUDGE_CA_FILE")),
		Insecure:   truthy(os.Getenv("EPHYRA_JUDGE_INSECURE")),
		MaxSlots:   defaultMaxSlots,
	}
	if s := strings.TrimSpace(os.Getenv("EPHYRA_JUDGE_MAX_SLOTS")); s != "" {
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil || n < 1 {
			return Options{}, fmt.Errorf("EPHYRA_JUDGE_MAX_SLOTS must be a positive integer")
		}
		opts.MaxSlots = int32(n)
	}
	if opts.Addr == "" {
		return Options{}, fmt.Errorf("EPHYRA_JUDGE_ADDR is required")
	}
	if opts.NodeID == "" {
		return Options{}, fmt.Errorf("EPHYRA_JUDGE_NODE_ID is required")
	}
	if opts.Credential == "" {
		return Options{}, fmt.Errorf("EPHYRA_JUDGE_CREDENTIAL is required")
	}
	if opts.WorkDir == "" {
		opts.WorkDir = defaultWorkDir
	}
	return opts, nil
}

// Run dials JudgeControl and serves the stream until ctx is cancelled.
// Stream errors reconnect with capped exponential backoff; running attempts
// and unsent results stay with the process, not the connection. A rejected
// handshake returns an error that includes reject_reason.
func Run(ctx context.Context, opts Options) error {
	opts, err := normalize(opts)
	if err != nil {
		return err
	}
	rt, err := newRuntime(ctx, opts)
	if err != nil {
		return err
	}
	defer rt.shutdown()
	backoff := minReconnectWait
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		slog.Info("connecting to judge control", "addr", opts.Addr, "node_id", opts.NodeID)
		handed, err := connectOnce(ctx, opts, rt)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, errHandshakeRejected) {
			slog.Warn("handshake rejected", "node_id", opts.NodeID, "reason", err.Error())
			return err
		}
		if handed {
			backoff = minReconnectWait
		}
		slog.Info("judge control stream ended; reconnecting",
			"node_id", opts.NodeID, "err", err, "backoff", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxReconnectWait {
			backoff = maxReconnectWait
		}
	}
}

func normalize(opts Options) (Options, error) {
	opts.Addr = strings.TrimSpace(opts.Addr)
	opts.NodeID = strings.TrimSpace(opts.NodeID)
	opts.WorkDir = strings.TrimSpace(opts.WorkDir)
	opts.CAFile = strings.TrimSpace(opts.CAFile)
	if opts.Addr == "" {
		return Options{}, fmt.Errorf("addr is required")
	}
	if opts.NodeID == "" {
		return Options{}, fmt.Errorf("node id is required")
	}
	if opts.Credential == "" {
		return Options{}, fmt.Errorf("credential is required")
	}
	if opts.Judge == nil {
		return Options{}, fmt.Errorf("judge function is required")
	}
	if len(opts.ExecutionProfileIDs) == 0 {
		return Options{}, fmt.Errorf("execution profile ids are required")
	}
	if opts.MaxSlots <= 0 {
		opts.MaxSlots = defaultMaxSlots
	}
	if opts.WorkDir == "" {
		opts.WorkDir = defaultWorkDir
	}
	return opts, nil
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func transportCreds(opts Options) (credentials.TransportCredentials, error) {
	if opts.Insecure {
		return insecure.NewCredentials(), nil
	}
	if opts.CAFile != "" {
		return credentials.NewClientTLSFromFile(opts.CAFile, "")
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}), nil
}

func dial(opts Options) (*grpc.ClientConn, error) {
	creds, err := transportCreds(opts)
	if err != nil {
		return nil, err
	}
	dopts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if opts.dialer != nil {
		dopts = append(dopts, grpc.WithContextDialer(opts.dialer))
	}
	return grpc.NewClient(opts.Addr, dopts...)
}

func rejectErr(reason string) error {
	if reason == "" {
		return errHandshakeRejected
	}
	return fmt.Errorf("%w: %s", errHandshakeRejected, reason)
}
