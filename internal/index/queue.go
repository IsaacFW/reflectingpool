package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// QueueSpec defines a review queue: which items are candidates, how they are
// grouped, and the order inside a group.
//
// Groups are worked through one at a time. With Groups ["share", "type"] and
// Order size descending, the queue serves one share's largest file type first,
// largest files first, and only moves to the next type once every item in the
// group has been annotated or skipped.
type QueueSpec struct {
	Filter Filter
	Groups []string // any of "share", "type", "ext", "folder"
	Order  Sort     // "size" (default, largest first), "modified" or "name"
	Limit  int
}

type GroupValue struct {
	Key   string `json:"key"`
	ID    int64  `json:"id,omitempty"`
	Label string `json:"label"`
}

type QueueGroup struct {
	Values    []GroupValue `json:"values"`
	Remaining int64        `json:"remaining"`
	Total     int64        `json:"total"`
	Size      int64        `json:"size"` // bytes across the whole group, reviewed or not

	raw  []any
	keys []string // keys[d] identifies the group's ancestor at depth d
}

type QueueResult struct {
	// Groups still holding unreviewed items, in the order they will be
	// served. Items come from the first.
	Groups     []QueueGroup `json:"groups"`
	GroupCount int          `json:"group_count"`
	Remaining  int64        `json:"remaining"`
	Items      []Row        `json:"items"`
}

var groupCols = map[string]string{
	"share":  "e.share",
	"type":   "e.cat",
	"ext":    "e.ext",
	"folder": "e.parent",
}

const (
	maxGroupsScanned  = 5000
	maxGroupsReturned = 50
)

// Queue returns the next unreviewed items for a review queue.
func (ix *Index) Queue(ctx context.Context, spec QueueSpec) (QueueResult, error) {
	res := QueueResult{Groups: []QueueGroup{}, Items: []Row{}}
	// Groups are ranked by everything in them, reviewed or not, so the order
	// does not shift under the user as they work through a group.
	spec.Filter.State = ""
	where, args, err := spec.Filter.where()
	if err != nil {
		return res, err
	}
	order, err := spec.Order.sql()
	if err != nil {
		return res, err
	}
	if spec.Limit <= 0 {
		spec.Limit = 20
	}
	cols := make([]string, len(spec.Groups))
	for i, g := range spec.Groups {
		col, ok := groupCols[g]
		if !ok {
			return res, QueryError(fmt.Sprintf("unknown group key %q", g))
		}
		cols[i] = col
	}

	q := `SELECT COALESCE(SUM(a.entry IS NULL), 0), COALESCE(SUM(e.size), 0), COUNT(*)`
	if len(cols) > 0 {
		q += ", " + strings.Join(cols, ", ")
	}
	q += rowFrom + where
	if len(cols) > 0 {
		q += " GROUP BY " + strings.Join(cols, ", ") + fmt.Sprintf(" ORDER BY 2 DESC LIMIT %d", maxGroupsScanned)
	}
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, err
	}
	var groups []QueueGroup
	for rows.Next() {
		g := QueueGroup{raw: make([]any, len(cols))}
		dest := []any{&g.Remaining, &g.Size, &g.Total}
		for i := range g.raw {
			dest = append(dest, &g.raw[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return res, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	// Rank with finished groups included, then drop them: a share must keep
	// its place after its biggest group is done.
	sortGroups(groups)
	pending := groups[:0]
	for _, g := range groups {
		if g.Remaining > 0 {
			pending = append(pending, g)
		}
	}
	groups = pending
	if len(groups) == 0 {
		return res, nil
	}
	res.GroupCount = len(groups)
	for _, g := range groups {
		res.Remaining += g.Remaining
	}
	if len(groups) > maxGroupsReturned {
		groups = groups[:maxGroupsReturned]
	}
	for i := range groups {
		if err := ix.labelGroup(ctx, spec.Groups, &groups[i]); err != nil {
			return res, err
		}
	}
	res.Groups = groups

	itemWhere, itemArgs := where+" AND a.entry IS NULL", args
	for i, col := range cols {
		itemWhere += " AND " + col + " = ?"
		itemArgs = append(itemArgs, groups[0].raw[i])
	}
	res.Items, err = ix.query(ctx, `SELECT `+rowCols+rowFrom+itemWhere+order+limitSQL(spec.Limit, 0), itemArgs...)
	if err != nil {
		return res, err
	}
	return res, ix.FillPaths(ctx, res.Items)
}

// sortGroups orders groups so that, at every level of grouping, the group
// holding the most bytes comes first: the biggest share, then within it the
// biggest file type, and so on.
func sortGroups(groups []QueueGroup) {
	if len(groups) == 0 {
		return
	}
	depth := len(groups[0].raw)
	totals := make([]map[string]int64, depth)
	for d := range totals {
		totals[d] = make(map[string]int64)
	}
	for i := range groups {
		g := &groups[i]
		g.keys = make([]string, depth)
		key := ""
		for d := range depth {
			key += fmt.Sprintf("%v\x00", g.raw[d])
			g.keys[d] = key
			totals[d][key] += g.Size
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := &groups[i], &groups[j]
		for d := range depth {
			if a.keys[d] == b.keys[d] {
				continue
			}
			if ta, tb := totals[d][a.keys[d]], totals[d][b.keys[d]]; ta != tb {
				return ta > tb
			}
			return a.keys[d] < b.keys[d]
		}
		return false
	})
}

func (ix *Index) labelGroup(ctx context.Context, keys []string, g *QueueGroup) error {
	g.Values = make([]GroupValue, len(keys))
	for i, key := range keys {
		v := GroupValue{Key: key}
		switch key {
		case "share":
			v.ID, _ = g.raw[i].(int64)
			v.Label = "(no share)"
			if v.ID != 0 {
				share, err := ix.Entry(ctx, v.ID)
				if err != nil {
					return err
				}
				v.Label = share.Name
			}
		case "type":
			cat, _ := g.raw[i].(int64)
			v.Label = CatName(int(cat))
		case "ext":
			v.Label, _ = g.raw[i].(string)
		case "folder":
			v.ID, _ = g.raw[i].(int64)
			path, err := ix.EntryPath(ctx, v.ID)
			if err != nil {
				return err
			}
			v.Label = path
		}
		g.Values[i] = v
	}
	return nil
}
