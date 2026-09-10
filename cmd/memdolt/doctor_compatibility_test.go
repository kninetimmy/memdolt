package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dolthub/dolt/go/cmd/dolt/doltversion"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/kninetimmy/memdolt/internal/embedding"
	"github.com/kninetimmy/memdolt/internal/hub"
	"github.com/kninetimmy/memdolt/internal/store"
	"github.com/kninetimmy/memdolt/internal/store/localdolt"
)

func TestDoctorEmbeddedReleaseEvidence(t *testing.T) {
	for _, version := range []string{doltversion.Version, "", "1.88.2", "0.40.17", "SET_BY_INIT", "__DOLT__"} {
		for _, asJSON := range []bool{false, true} {
			check := embeddedDoltCheck(version)
			cmd := &cobra.Command{Use: "doctor"}
			var out bytes.Buffer
			cmd.SetOut(&out)
			jsonOutput = asJSON
			err := finishDoctorReport(cmd, doctorReport{Checks: []doctorCheck{check}})
			if (err == nil) != (version == hub.Version) || !strings.Contains(out.String(), hub.Version) {
				t.Fatalf("version %q: output %q, error %v", version, &out, err)
			}
			if asJSON && decodeDoctorReport(t, out.String()).OK != (err == nil) {
				t.Fatal("JSON hid failed embedded release evidence")
			}
		}
	}
	actual := findCheck(t, runDoctorJSON(t, scratchDir(t)), "embedded-dolt-version")
	if actual.Status != statusOK || !strings.Contains(actual.Detail, doltversion.Version) || !strings.Contains(actual.Detail, "doltversion.Version") {
		t.Fatalf("running binary release = %+v", actual)
	}
}

func TestDoctorHubAndSelectionFailuresPreserveMemory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	bundle := filepath.Join(base, "bundle")
	cfg := hub.DefaultConfig()
	cfg.IPv4, cfg.IPv6 = "100.90.0.1", "fd7a:115c:a1e0::1"
	if _, err := hub.Init(bundle, cfg); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(bundle, "hub.json")
	invalid := filepath.Join(base, "invalid.json")
	writeTestFile(t, invalid, "{}")
	for _, tc := range []struct {
		args  []string
		check string
	}{
		{[]string{"--hub"}, "selection"},
		{[]string{"--config", selected}, "selection"},
		{[]string{"--hub", "--config", selected, "--dir", base}, "selection"},
		{[]string{"--hub", "--config", selected, "--remote", "origin"}, "selection"},
		{[]string{"--hub", "--config", selected, "--user", "fixture"}, "selection"},
		{[]string{"--remote="}, "selection"},
		{[]string{"--user", "fixture"}, "selection"},
		{[]string{"--remote", "origin", "--user="}, "selection"},
		{[]string{"--remote", "../remote"}, "selection"},
		{[]string{"--remote", "origin", "--user", "-invalid"}, "selection"},
		{[]string{"--hub", "--config", "relative.json"}, "hub-configuration"},
		{[]string{"--hub", "--config", filepath.Join(base, "missing.json")}, "hub-configuration"},
		{[]string{"--hub", "--config", invalid}, "hub-configuration"},
		{[]string{"--hub", "--config", selected}, ""},
	} {
		for _, asJSON := range []bool{false, true} {
			args := append([]string{"doctor"}, tc.args...)
			if asJSON {
				args = append(args, "--json")
			}
			out, err := runMemdoltResult(t, args...)
			if err == nil || !strings.Contains(out, statusFail) || !strings.Contains(out, tc.check) {
				t.Fatalf("%v: %q, %v", args, out, err)
			}
			if asJSON {
				report := decodeDoctorReport(t, out)
				if report.OK || report.Dir != "" || report.Remote != nil {
					t.Fatalf("hub/selection reached repository checks: %+v", report)
				}
				if tc.check == "" && runtime.GOOS != "linux" {
					if findCheck(t, report, "hub-platform").Status != statusFail || report.Hub.Version != "" || report.Hub.Platform != runtime.GOOS {
						t.Fatalf("unsupported platform evidence = %+v", report)
					}
					if findCheck(t, report, "hub-version").Status != "unknown" {
						t.Fatal("unobserved hub release was treated as observed")
					}
				}
			}
		}
	}
	if _, err := os.Stat(pathsFor(t, base).Dir()); !os.IsNotExist(err) {
		t.Fatalf("hub/selection diagnostics created repository memory: %v", err)
	}
	for _, flag := range []string{"--password", "--global", "--diff"} {
		if err := runMemdoltErr(t, "doctor", flag); !strings.Contains(err, "unknown flag") {
			t.Fatalf("unexpected diagnostic input accepted: %s", flag)
		}
	}
}

func TestDoctorRemoteRefusesAbsentAndPartialStores(t *testing.T) {
	for _, suffix := range []string{"", ".memdolt", ".memdolt/dolt", ".memdolt/dolt/memory/.dolt/noms"} {
		base := scratchDir(t)
		if suffix != "" {
			if err := os.MkdirAll(filepath.Join(base, suffix), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		before := repoFiles(t, base)
		for _, asJSON := range []bool{false, true} {
			args := []string{"doctor", "--dir", base, "--remote", "origin"}
			if asJSON {
				args = append(args, "--json")
			}
			out, err := runMemdoltResult(t, args...)
			if err == nil || !strings.Contains(out, "memdolt init") || !strings.Contains(out, "remote-compatibility") {
				t.Fatalf("partial %s: %q, %v", suffix, out, err)
			}
			if asJSON && decodeDoctorReport(t, out).OK {
				t.Fatal("absent remote store reported healthy")
			}
		}
		if !reflect.DeepEqual(repoFiles(t, base), before) {
			t.Fatal("remote diagnostics initialized a partial store")
		}
	}
}

func TestDoctorOfflineAndRemoteTransportRefusal(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			const password = "synthetic-doctor-owner-password"
			t.Setenv("DOLT_REMOTE_PASSWORD", password)
			var requests atomic.Int32
			headers := make(chan string, 8)
			remote := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
				requests.Add(1)
				md, _ := metadata.FromIncomingContext(stream.Context())
				header := strings.Join(md.Get("authorization"), " ")
				select {
				case headers <- header:
				default:
				}
				return status.Error(codes.Unauthenticated, "fixture transport refusal "+password+" "+header)
			}))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- remote.Serve(listener) }()
			defer func() { remote.Stop(); <-done }()
			base := initStore(t)
			runMemdolt(t, "repo", "remote", "add", "origin", "http://"+listener.Addr().String()+"/memory", "--user", "stored", "--dir", base)
			if routed {
				serveTransferProcess(t, base)
				// Only the already running owner's environment may authenticate.
				t.Setenv("DOLT_REMOTE_PASSWORD", "synthetic-doctor-client-password")
			}
			for _, asJSON := range []bool{false, true} {
				args := []string{"doctor", "--dir", base}
				if asJSON {
					args = append(args, "--json")
				}
				out := runMemdolt(t, args...)
				if requests.Load() != 0 || strings.Contains(out, "remote-compatibility") || strings.Contains(out, "remote-dolt-version") {
					t.Fatal("ordinary doctor attempted remote inspection")
				}
			}
			for _, asJSON := range []bool{false, true} {
				args := []string{"doctor", "--dir", base, "--remote", "origin", "--user", "fixture"}
				if asJSON {
					args = append(args, "--json")
				}
				out, err := runMemdoltResult(t, args...)
				if err == nil || !strings.Contains(out, "fetch remote main") || !strings.Contains(out, "release unobserved") {
					t.Fatalf("transport refusal = %q, %v", out, err)
				}
				want := "Basic " + base64.StdEncoding.EncodeToString([]byte("fixture:"+password))
				if strings.Contains(out, password) || strings.Contains(out, want) {
					t.Fatal("doctor leaked fixture credentials")
				}
				select {
				case header := <-headers:
					if header != want {
						t.Fatal("doctor did not use username override and executing-owner password")
					}
				default:
					t.Fatal("remote credential fixture was not reached")
				}
				if asJSON && findCheck(t, decodeDoctorReport(t, out), "remote-compatibility").Status != statusFail {
					t.Fatal("transport refusal reported compatible")
				}
			}
			if requests.Load() == 0 {
				t.Fatal("explicit remote diagnostic never reached the selected fixture")
			}
		})
	}
}

func TestDoctorRemoteCommittedCompatibilityDirectAndOwner(t *testing.T) {
	for _, routed := range []bool{false, true} {
		t.Run(fmt.Sprintf("owner=%t", routed), func(t *testing.T) {
			ctx := context.Background()
			source := scratchDir(t)
			identityCLIGit(t, source, "https://github.com/fixture/doctor")
			runMemdolt(t, "init", "--dir", source)
			remote := configureCLITransferRemote(t, source)
			runMemdolt(t, "push", "--dir", source)
			base := scratchDir(t)
			runMemdolt(t, "clone", remote, "--dir", base)
			runMemdolt(t, "repo", "remote", "add", "mirror", remote, "--dir", base)
			runMemdolt(t, "repo", "configure", "--topology", "local", "--dir", base)
			st := openInitializedStore(t, base)
			if _, err := st.ProposeFact(ctx, localdolt.Proposal{Actor: cliStagingActor, Rationale: "doctor fixture", Target: localdolt.TargetRepo},
				localdolt.Fact{Key: "doctor.pending", Value: "unapproved fixture"}); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			db := openRepoFixtureDB(t, base)
			for _, statement := range []string{
				"CALL DOLT_TAG('--author', 'Fixture <fixture@example.invalid>', '-m', 'preserve', 'doctor-keep')",
				"INSERT INTO meta (k, v) VALUES ('doctor_dirty', 'staged')",
				"CALL DOLT_ADD('meta')",
				"UPDATE meta SET v = 'working' WHERE k = 'doctor_dirty'",
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			tags := transferTags(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			paths := pathsFor(t, base)
			if err := embedding.RecordRecall(ctx, paths.EmbeddingsFile(), true); err != nil {
				t.Fatal(err)
			}
			codeIndex := filepath.Join(paths.Dir(), "code_index.sqlite")
			writeTestFile(t, codeIndex, "fixture code index")
			files := map[string][]byte{}
			for _, path := range []string{paths.ConfigFile(), paths.EmbeddingsFile(), codeIndex} {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				files[path] = body
			}
			st = openInitializedStore(t, base)
			before := repoStateSnapshot(t, st)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			runMemdolt(t, "note", "add", "new remote fixture", "--dir", source)
			remoteHead := decodeJSON[localdolt.TransferResult](t, runMemdolt(t, "push", "--dir", source, "--json")).MainCommit
			var stop func()
			if routed {
				stop = serveTransferProcess(t, base)
			}
			for _, asJSON := range []bool{false, true} {
				args := []string{"doctor", "--dir", base, "--remote", "mirror"}
				if asJSON {
					args = append(args, "--json")
				}
				out := runMemdolt(t, args...)
				for _, want := range []string{"remote mirror reachable", remoteHead, "matching committed project identity", "behind", "release unobserved", "doctor --hub --config"} {
					if !strings.Contains(out, want) {
						t.Fatalf("remote evidence lacks %q: %s", want, out)
					}
				}
				if asJSON {
					report := decodeDoctorReport(t, out)
					if !report.OK || report.Remote.RemoteCommit != remoteHead || report.Remote.Remote != "mirror" || report.Remote.Clean ||
						report.Remote.PendingProposals.Repo != 1 || findCheck(t, report, "remote-compatibility").Status != statusOK ||
						findCheck(t, report, "remote-dolt-version").Status != statusWarn {
						t.Fatalf("incorrect remote evidence: %+v", report)
					}
				}
			}
			for _, refusal := range []string{"missing", "schema", "identity"} {
				name, want := "mirror", refusal
				if refusal == "missing" {
					name, want = "missing", "not configured"
				} else {
					db := openRepoFixtureDB(t, source)
					version := store.LatestSchemaVersion()
					if refusal == "schema" {
						version++
					} else if _, err := db.Exec("DELETE FROM meta WHERE k IN ('project_id', 'project_origin')"); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec("UPDATE meta SET v = ? WHERE k = ?", fmt.Sprint(version), store.SchemaVersionKey); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec("CALL DOLT_COMMIT('-am', 'fixture incompatible remote')"); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec("CALL DOLT_PUSH('origin', 'main')"); err != nil {
						t.Fatal(err)
					}
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				for _, asJSON := range []bool{false, true} {
					args := []string{"doctor", "--dir", base, "--remote", name}
					if asJSON {
						args = append(args, "--json")
					}
					out, err := runMemdoltResult(t, args...)
					if err == nil || !strings.Contains(out, want) || !strings.Contains(out, "release unobserved") {
						t.Fatalf("%s refusal: %q, %v", refusal, out, err)
					}
					if asJSON && findCheck(t, decodeDoctorReport(t, out), "remote-compatibility").Status != statusFail {
						t.Fatalf("%s reported compatible", refusal)
					}
				}
			}
			if stop != nil {
				stop()
			}
			st = openInitializedStore(t, base)
			if after := repoStateSnapshot(t, st); !reflect.DeepEqual(before, after) {
				t.Fatalf("doctor changed branches or working/staged roots: %v -> %v", before, after)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			db = openRepoFixtureDB(t, base)
			if !reflect.DeepEqual(tags, transferTags(t, db)) {
				t.Fatal("doctor changed tags")
			}
			for path, before := range files {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("doctor changed local artifact %s: %v", path, err)
				}
			}
		})
	}
}

func TestDoctorRemoteRetainsLateCloseEvidence(t *testing.T) {
	base := initStore(t)
	configureCLITransferRemote(t, base)
	runMemdolt(t, "push", "--dir", base)
	late := errors.New("fixture late close failure")
	st := &repoFaultStore{commandStore: &localCommandStore{Store: openInitializedStore(t, base), baseDir: base}, closeErr: late}
	check, observed := doctorRemoteCheck(context.Background(), st, localdolt.RepoStatusOptions{Remote: "origin"})
	if !st.closed || check.Status != statusFail || !strings.Contains(check.Detail, late.Error()) || observed.RemoteCommit == "" || observed.Status != "current" {
		t.Fatalf("late close hid observations: %+v, %+v", check, observed)
	}
	for _, asJSON := range []bool{false, true} {
		cmd := &cobra.Command{Use: "doctor"}
		var out bytes.Buffer
		cmd.SetOut(&out)
		jsonOutput = asJSON
		err := finishDoctorReport(cmd, doctorReport{Checks: []doctorCheck{check}, Remote: observed})
		if err == nil || !strings.Contains(out.String(), observed.RemoteCommit) || !strings.Contains(out.String(), late.Error()) {
			t.Fatalf("late close report lost captured hash/error: %q, %v", &out, err)
		}
	}
	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetOut(repoFailWriter{late})
	if err := finishDoctorReport(cmd, doctorReport{Checks: []doctorCheck{check}, Remote: observed}); !errors.Is(err, late) || !strings.Contains(err.Error(), "checks failed") {
		t.Fatalf("output error hid failed diagnostic: %v", err)
	}
}
