package localdolt

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kninetimmy/memdolt/internal/memory"
	"github.com/kninetimmy/memdolt/internal/store"
)

func TestGlobalDocumentsRetainNativeLateResultsAndSeparateConfig(t *testing.T) {
	for _, populated := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("populated=%t/canceled=%t", populated, canceled), func(t *testing.T) {
				repoA, global := globalStoreFixture(t)
				defer func() { _ = global.Close() }()
				ctx := context.Background()
				wantDocs, wantChunks := 0, 0
				if populated {
					file := filepath.Join(interopTempDir(t), "prior.md")
					writeDocumentFixture(t, file, "# Previously shared\n")
					prior, err := global.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor})
					if err != nil {
						t.Fatal(err)
					}
					wantDocs, wantChunks = 1, len(prior.Chunks)
				}
				repoB, _ := documentFixture(t)
				repoC, _ := documentFixture(t)
				for _, repo := range []*Store{repoB, repoC} {
					if _, err := SetGlobalEnabled(repo.paths.Base(), true); err != nil {
						t.Fatal(err)
					}
				}
				openFor := func(repo *Store) {
					t.Helper()
					if err := global.Close(); err != nil {
						t.Fatal(err)
					}
					var err error
					global, err = OpenGlobal(ctx, repo.paths.Base())
					if err != nil {
						t.Fatal(err)
					}
				}
				openFor(repoB)
				before := internalCount(t, global, "SELECT COUNT(*) FROM dolt_log")
				file := filepath.Join(interopTempDir(t), "global-native.md")
				writeDocumentFixture(t, file, "# Global native result\n\nDurable first chunk.\n\n## Detail\n\nDurable second chunk.\n")
				addCtx, cancelAdd := context.WithCancel(ctx)
				defer cancelAdd()
				configCalls := 0
				added, err := global.docAddFinalize(addCtx, DocAddOptions{File: file, Actor: memory.UserActor}, func(root *os.Root) (bool, error) {
					configCalls++
					return enableGlobalDocumentRecall(root)
				}, func(tx *sql.Tx) error {
					if canceled {
						cancelAdd()
						return tx.Commit()
					}
					return errors.Join(tx.Commit(), errConfirmed)
				})
				requireConfirmed(t, added.Commit, err)
				if errors.Is(err, store.ErrCommitUnknown) || added.Status != "created" || added.Document == nil || len(added.Chunks) != 2 || added.EnabledDefaultRecall || configCalls != 0 {
					t.Fatalf("native global add lost evidence or finalized config after error: %+v, %v (config calls=%d)", added, err, configCalls)
				}
				if !strings.Contains(err.Error(), "[global] include_docs_in_default") || strings.Contains(err.Error(), "[retrieval]") || !strings.Contains(err.Error(), "do not replay ingestion") {
					t.Fatalf("late add has the wrong scope/remedy: %v", err)
				}
				openFor(repoB)
				shown, err := global.DocShow(ctx, added.Document.ID)
				if err != nil || !reflect.DeepEqual(shown.Document, added.Document) || !reflect.DeepEqual(shown.Chunks, added.Chunks) {
					t.Fatalf("reopened native add disagrees with confirmed result: %+v, %v", shown, err)
				}
				if internalCount(t, global, "SELECT COUNT(*) FROM dolt_log") != before+1 {
					t.Fatal("late native add duplicated or lost history")
				}
				// C explicitly selects the already shared document. This is not a
				// replay of B's failed ingestion or a repair of B's config.
				openFor(repoC)
				unchanged, err := global.DocAdd(ctx, DocAddOptions{File: file, Actor: memory.UserActor})
				if err != nil || unchanged.Status != "unchanged" || unchanged.Commit != "" || !unchanged.EnabledDefaultRecall {
					t.Fatalf("C's config-only result=%+v, %v", unchanged, err)
				}
				for _, check := range []struct {
					repo *Store
					docs bool
				}{{repoA, populated}, {repoB, false}, {repoC, true}} {
					cfg, err := ReadGlobalConfig(check.repo.paths.Base())
					if err != nil || !cfg.Enabled || cfg.IncludeDocsInDefault != check.docs {
						t.Fatalf("global config crossed repository scopes: %+v, %v", cfg, err)
					}
				}
				removeCtx, cancelRemove := context.WithCancel(ctx)
				defer cancelRemove()
				removed, err := global.docRemove(removeCtx, added.Document.ID, memory.UserActor, func(tx *sql.Tx) error {
					if canceled {
						cancelRemove()
						return tx.Commit()
					}
					return errors.Join(tx.Commit(), errConfirmed)
				})
				requireConfirmed(t, removed.Commit, err)
				if errors.Is(err, store.ErrCommitUnknown) || removed.Status != "removed" || !reflect.DeepEqual(removed.Document, added.Document) || removed.Commit == added.Commit {
					t.Fatalf("native global removal lost identity/hash: %+v, %v", removed, err)
				}
				openFor(repoC)
				if shown, err := global.DocShow(ctx, added.Document.ID); err != nil || shown.Status != "not-found" {
					t.Fatalf("late removal did not persist: %+v, %v", shown, err)
				}
				if internalCount(t, global, "SELECT COUNT(*) FROM documents AS OF 'main'") != wantDocs || internalCount(t, global, "SELECT COUNT(*) FROM doc_chunks AS OF 'main'") != wantChunks || internalCount(t, global, "SELECT COUNT(*) FROM dolt_log") != before+2 || internalCount(t, global, "SELECT COUNT(*) FROM dolt_status") != 0 {
					t.Fatal("native late results/config-only work lost or duplicated durable changes")
				}
			})
		}
	}
}

// Finalizer errors above occur after an observed hash and must stay confirmed.
// This separately exercises #145's shared native-result seam when document
// writes committed but cancellation prevents observing their result row.
func TestGlobalDocumentNativeUnobservedResultsRemainUnknown(t *testing.T) {
	for _, operation := range []string{"add", "remove"} {
		t.Run(operation, func(t *testing.T) {
			repo, global := globalStoreFixture(t)
			defer func() { _ = global.Close() }()
			file, body := filepath.Join(interopTempDir(t), "unobserved.md"), "# Unobserved global document\n"
			id := newID()
			if operation == "remove" {
				writeDocumentFixture(t, file, body)
				added, err := global.DocAdd(context.Background(), DocAddOptions{File: file, Actor: memory.UserActor})
				if err != nil {
					t.Fatal(err)
				}
				id = added.Document.ID
			}
			before := internalCount(t, global, "SELECT COUNT(*) FROM dolt_log")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, err := global.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if operation == "add" {
				_, err = tx.ExecContext(ctx, "INSERT INTO documents(id, path, title, content_hash, byte_len, source, ingested_at) VALUES (?, ?, 'Unobserved', ?, ?, 'user', NOW())", id, file, fmt.Sprintf("%x", sha256.Sum256([]byte(body))), len(body))
				if err == nil {
					_, err = tx.ExecContext(ctx, "INSERT INTO doc_chunks(id, doc_id, ord, heading_path, body) VALUES (?, ?, 0, 'Unobserved', ?)", newID(), id, body)
				}
			} else {
				_, err = tx.ExecContext(ctx, "DELETE FROM doc_chunks WHERE doc_id = ?", id)
				if err == nil {
					_, err = tx.ExecContext(ctx, "DELETE FROM documents WHERE id = ?", id)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err := tx.QueryContext(ctx, "CALL DOLT_COMMIT('-A', '-m', ?, '--author', ?)", "unobserved global document "+operation, memory.UserActor.CommitAuthor().String())
			if err != nil || !rows.Next() {
				t.Fatalf("native document commit did not return a row: %v", err)
			}
			cancel()
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			var hash string
			scanErr := rows.Scan(&hash)
			result, err := nativeCommitResult(hash, 2, scanErr)
			err = documentFinalError(DocResult{}, err)
			if scanErr == nil || result.Hash != "" || !errors.Is(err, store.ErrCommitUnknown) || strings.Contains(err.Error(), "confirmed") {
				t.Fatalf("unobserved global document result claimed a known outcome: %+v, %v", result, err)
			}
			_ = tx.Rollback()
			_ = conn.Close()
			if err := global.Close(); err != nil {
				t.Fatal(err)
			}
			global, err = OpenGlobal(context.Background(), repo.paths.Base())
			if err != nil {
				t.Fatal(err)
			}
			shown, err := global.DocShow(context.Background(), id)
			if err != nil || operation == "add" && (shown.Document == nil || len(shown.Chunks) != 1) || operation == "remove" && shown.Status != "not-found" {
				t.Fatalf("unobserved commit was not durable after reopen: %+v, %v", shown, err)
			}
			if internalCount(t, global, "SELECT COUNT(*) FROM dolt_log") != before+1 || internalCount(t, global, "SELECT COUNT(*) FROM dolt_status") != 0 {
				t.Fatal("unobserved native document outcome was lost or replayed")
			}
		})
	}
}
