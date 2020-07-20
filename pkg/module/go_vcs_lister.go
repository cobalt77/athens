package module

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gomods/athens/pkg/auth"
	"github.com/gomods/athens/pkg/config"
	"github.com/gomods/athens/pkg/errors"
	"github.com/gomods/athens/pkg/log"
	"github.com/gomods/athens/pkg/observ"
	"github.com/gomods/athens/pkg/storage"
	"github.com/spf13/afero"
)

type listResp struct {
	Path     string
	Version  string
	Versions []string `json:",omitempty"`
	Time     time.Time
}

type vcsLister struct {
	goBinPath             string
	env                   []string
	fs                    afero.Fs
	propagateAuth         bool
	propagateAuthPatterns []string
}

// NewVCSLister creates an UpstreamLister which uses VCS to fetch a list of available versions
func NewVCSLister(goBinPath string, env []string, fs afero.Fs, propagateAuth bool, propagateAuthPatterns []string) UpstreamLister {
	return &vcsLister{
		goBinPath:             goBinPath,
		env:                   env,
		fs:                    fs,
		propagateAuth:         propagateAuth,
		propagateAuthPatterns: propagateAuthPatterns,
	}
}

func (l *vcsLister) shouldPropAuth(module string) bool {
	return l.propagateAuth && matchesAuthPattern(l.propagateAuthPatterns, module)
}

func (l *vcsLister) List(ctx context.Context, module string) (*storage.RevInfo, []string, error) {
	const op errors.Op = "vcsLister.List"
	ctx, span := observ.StartSpan(ctx, op.String())
	defer span.End()
	var (
		netrcDir string
		err      error
	)
	creds, ok := auth.FromContext(ctx)
	if ok && l.shouldPropAuth(module) {
		log.EntryFromContext(ctx).Debugf("propagating authentication")
		host := strings.Split(module, "/")[0]
		netrcDir, err = auth.WriteTemporaryNETRC(host, creds.User, creds.Password)
		if err != nil {
			return nil, nil, errors.E(op, err)
		}
		defer os.RemoveAll(netrcDir)
	}
	tmpDir, err := afero.TempDir(l.fs, "", "go-list")
	if err != nil {
		return nil, nil, errors.E(op, err)
	}
	defer l.fs.RemoveAll(tmpDir)

	cmd := exec.Command(
		l.goBinPath,
		"list", "-m", "-versions", "-json",
		config.FmtModVer(module, "latest"),
	)
	cmd.Dir = tmpDir
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	gopath, err := afero.TempDir(l.fs, "", "athens")
	if err != nil {
		return nil, nil, errors.E(op, err)
	}
	defer clearFiles(l.fs, gopath)
	cmd.Env = prepareEnv(gopath, netrcDir, l.env)

	err = cmd.Run()
	if err != nil {
		err = fmt.Errorf("%v: %s", err, stderr)
		// as of now, we can't recognize between a true NotFound
		// and an unexpected error, so we choose the more
		// hopeful path of NotFound. This way the Go command
		// will not log en error and we still get to log
		// what happened here if someone wants to dig in more.
		// Once, https://github.com/golang/go/issues/30134 is
		// resolved, we can hopefully differentiate.
		return nil, nil, errors.E(op, err, errors.KindNotFound)
	}

	var lr listResp
	err = json.NewDecoder(stdout).Decode(&lr)
	if err != nil {
		return nil, nil, errors.E(op, err)
	}
	rev := storage.RevInfo{
		Time:    lr.Time,
		Version: lr.Version,
	}
	return &rev, lr.Versions, nil
}
