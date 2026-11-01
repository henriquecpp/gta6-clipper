package detect

import (
	"slices"
	"time"

	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

// ClipsConfig configures the viewer clip ("clip rain") detector.
type ClipsConfig struct {
	// GroupWindow groups clips whose positions fall within ± this duration.
	GroupWindow time.Duration
	// MinClips is how many clips make a cluster.
	MinClips int
	// Retention is how much history is kept.
	Retention time.Duration
}

// Clips is the late signal: viewer clips arrive minutes after the moment,
// so they confirm and calibrate rather than trigger previews.
type Clips struct {
	cfg  ClipsConfig
	seen map[string]domain.StreamPos
	pos  []domain.StreamPos // sorted
}

var _ Detector = (*Clips)(nil)

// NewClips builds a clip detector.
func NewClips(cfg ClipsConfig) *Clips {
	if cfg.GroupWindow <= 0 {
		cfg.GroupWindow = 15 * time.Second
	}
	if cfg.MinClips < 1 {
		cfg.MinClips = 3
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 15 * time.Minute
	}
	return &Clips{cfg: cfg, seen: make(map[string]domain.StreamPos)}
}

// Kind returns SignalClips.
func (c *Clips) Kind() domain.SignalKind { return domain.SignalClips }

// Advance drops clips older than the retention.
func (c *Clips) Advance(pos domain.StreamPos) {
	cut := pos - domain.StreamPos(c.cfg.Retention)
	i := 0
	for i < len(c.pos) && c.pos[i] < cut {
		i++
	}
	if i == 0 {
		return
	}
	c.pos = slices.Delete(c.pos, 0, i)
	for id, p := range c.seen {
		if p < cut {
			delete(c.seen, id)
		}
	}
}

// Observe records a clip; it returns false if the clip id was already seen.
func (c *Clips) Observe(id string, pos domain.StreamPos) bool {
	if _, dup := c.seen[id]; dup {
		return false
	}
	c.seen[id] = pos
	i, _ := slices.BinarySearch(c.pos, pos)
	c.pos = slices.Insert(c.pos, i, pos)
	return true
}

// Value is the number of clips within ± GroupWindow of pos.
func (c *Clips) Value(pos domain.StreamPos) float64 {
	return float64(c.count(pos))
}

func (c *Clips) count(pos domain.StreamPos) int {
	w := domain.StreamPos(c.cfg.GroupWindow)
	lo, _ := slices.BinarySearch(c.pos, pos-w)
	hi, found := slices.BinarySearch(c.pos, pos+w)
	for found && hi < len(c.pos) && c.pos[hi] == pos+w {
		hi++
	}
	return hi - lo
}

// Cluster is a group of clips around a position.
type Cluster struct {
	Center domain.StreamPos
	Count  int
}

// Clusters returns non-overlapping groups with at least MinClips clips. Each
// cluster is centred on the median of its clips; groups are formed greedily
// from the densest position.
func (c *Clips) Clusters() []Cluster {
	used := make([]bool, len(c.pos))
	w := domain.StreamPos(c.cfg.GroupWindow)
	var out []Cluster
	for {
		best, bestN := -1, 0
		for i, p := range c.pos {
			if used[i] {
				continue
			}
			n := 0
			for j, q := range c.pos {
				if !used[j] && q >= p-w && q <= p+w {
					n++
				}
			}
			if n > bestN {
				best, bestN = i, n
			}
		}
		if best < 0 || bestN < c.cfg.MinClips {
			break
		}
		p := c.pos[best]
		var members []domain.StreamPos
		for j, q := range c.pos {
			if !used[j] && q >= p-w && q <= p+w {
				members = append(members, q)
				used[j] = true
			}
		}
		out = append(out, Cluster{Center: members[len(members)/2], Count: len(members)})
	}
	slices.SortFunc(out, func(a, b Cluster) int { return int(a.Center - b.Center) })
	return out
}
