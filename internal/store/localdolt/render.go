package localdolt

import (
	"context"

	"github.com/kninetimmy/memdolt/internal/render"
)

// Render captures committed main and replaces its local Markdown views inside
// the owning process. It never changes memory, branches, or pending notes.
func (s *Store) Render(ctx context.Context) (render.Result, error) {
	// ponytail: share the existing mutation boundary through the file operation;
	// shorten it to capture alone if large render output delays memory writes.
	// Foreign Dolt sessions do not share this mutex; all reads pin a commit hash.
	s.proposalMu.Lock()
	defer s.proposalMu.Unlock()
	return render.Run(ctx, s.paths.Base(), s)
}
