package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// MetaDir is the name of the metadata folder at the root of each share. It is
// ours, not something for the user to describe, so no queue offers it.
const MetaDir = ".reflection"

// QueueSpec defines a review queue: which items are candidates, how they are
// grouped, and the order inside a group.
//
// Groups are worked through one at a time. With Groups ["share", "type"] and
// Order size descending, the queue serves one share's largest file type first,
// largest files first, and only moves to the next type once every item in the
// group has been annotated or skipped.
type QueueSpec struct {
	Filter Filter
	Groups []string // any of "share", "type", "ext", "folder", "age"
	Order  Sort     // "size" (default, largest first), "modified", "created", "age" or "name"
	Limit  int
	// Offset passes over that many unreviewed items from the head of the
	// queue. It is how a queue is paged where nothing ever leaves it, as in
	// read-only mode.
	Offset int
	// ExcludeCovered counts an item inside a described folder as reviewed.
	ExcludeCovered bool
}

type GroupValue struct {
	Key string `json:"key"`
	// Value identifies the group: an entry ID for "share" and "folder", a
	// name for the other keys. It is what a client sends back to name the
	// group.
	Value any    `json:"value"`
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

// key identifies the group among all groups of one queue.
func (g *QueueGroup) key() string {
	if len(g.keys) == 0 {
		return ""
	}
	return g.keys[len(g.keys)-1]
}

type QueueResult struct {
	// Groups still holding unreviewed items, in the order they will be
	// served, starting with the one Items come from.
	Groups []QueueGroup `json:"groups"`
	// GroupCount is how many groups still hold unreviewed items. GroupTotal
	// counts the finished ones too, and GroupPosition is the place of the
	// current group among them: "group 3 of 31".
	GroupCount    int   `json:"group_count"`
	GroupTotal    int   `json:"group_total"`
	GroupPosition int   `json:"group_position"`
	Remaining     int64 `json:"remaining"`
	// Total and TotalSize cover every item of the queue, reviewed or not.
	Total     int64 `json:"total"`
	TotalSize int64 `json:"total_size"`
	Items     []Row `json:"items"`
}

const (
	maxGroupsScanned  = 5000
	maxGroupsReturned = 50
	// Every remembered queue is brought up to date at each save, so only the
	// few most recently used are kept.
	maxCachedQueues = 16

	coveredSQL = `EXISTS (SELECT 1 FROM covered c WHERE c.dir = e.parent)`
)

// headSize is how many of a group's next items are held ready.
var headSize = 2000

// Age buckets, oldest first, which is the order they are served in.
type ageBucket struct {
	name, label   string
	years, months int // the bucket holds what is older than this
}

var ageBuckets = []ageBucket{
	{"over-5y", "Over 5 years", 5, 0},
	{"2y-5y", "2 to 5 years", 2, 0},
	{"1y-2y", "1 to 2 years", 1, 0},
	{"6m-1y", "6 to 12 months", 0, 6},
	{"under-6m", "Under 6 months", 0, 0},
}

// ageBucketSQL is the expression for an entry's place in ageBuckets.
func ageBucketSQL(scanned time.Time) string {
	var b strings.Builder
	b.WriteString("(CASE")
	last := len(ageBuckets) - 1
	for i, bucket := range ageBuckets[:last] {
		fmt.Fprintf(&b, " WHEN %s < %d THEN %d", newestExpr, scanned.AddDate(-bucket.years, -bucket.months, 0).Unix(), i)
	}
	fmt.Fprintf(&b, " ELSE %d END)", last)
	return b.String()
}

func (ix *Index) groupCol(key string) (string, bool) {
	switch key {
	case "share":
		return "e.share", true
	case "type":
		return "e.cat", true
	case "ext":
		return "e.ext", true
	case "folder":
		return "e.parent", true
	case "age":
		return ix.ageCol, true
	}
	return "", false
}

// queueState is what the index remembers about one queue definition between
// calls. Queue runs after every item a user reviews, so a call must not cost
// more as the index or the number of reviewed items grows. The groups and
// their order are worked out once; the counts of reviewed items are adjusted
// as annotations change instead of being recounted; and the next items of the
// current group are held ready, so the index is searched once per couple of
// thousand items instead of once per item.
type queueState struct {
	spec  QueueSpec
	cols  []string
	where string // the queue's candidates
	args  []any
	// The same without the subtree condition, which is costly to apply to a
	// single entry and is checked by walking up from the entry instead.
	oneWhere string
	oneArgs  []any

	groups      []QueueGroup // every group, in serving order
	total, size int64

	done map[string]int64 // reviewed candidates per group; nil when not counted
	cov  map[string]int64 // unreviewed candidates in covered folders; nil when not counted
	head *queueHead
	used int64 // when the queue was last asked for, by the index's count of calls
}

// queueHead holds the next items of one group.
type queueHead struct {
	group, order string
	ids          []int64 // unreviewed when fetched, in serving order
	full         bool    // ids held every unreviewed item of the group
}

func queueKey(spec QueueSpec) string {
	spec.Filter.State, spec.Filter.Prefix = "", ""
	key, _ := json.Marshal(struct {
		F Filter
		G []string
		C bool
	}{spec.Filter, spec.Groups, spec.ExcludeCovered})
	return string(key)
}

// candidates returns the WHERE clause for the items a queue may serve.
func candidates(f Filter) (string, []any, error) {
	// Review state is applied separately; an annotation-based filter has no
	// meaning for items that are, by definition, not annotated yet.
	f.State, f.Prefix = "", ""
	where, args, err := f.where()
	if err != nil {
		return "", nil, err
	}
	// Only what is inside a share can be annotated or skipped. Anything else
	// would sit at the head of the queue for ever.
	where += " AND e.share != 0"
	if f.Kind != "file" {
		where += " AND NOT (e.kind = 1 AND e.parent = e.share AND e.name = '" + MetaDir + "')"
	}
	return where, args, nil
}

// queueFor returns the remembered state of a queue, working out its groups
// the first time the queue is seen. The caller holds qmu.
func (ix *Index) queueFor(ctx context.Context, spec QueueSpec) (*queueState, error) {
	key := queueKey(spec)
	ix.queueCalls++
	if st, ok := ix.queues[key]; ok {
		st.used = ix.queueCalls
		return st, nil
	}
	st := &queueState{spec: spec, used: ix.queueCalls}
	for _, g := range spec.Groups {
		col, ok := ix.groupCol(g)
		if !ok {
			return nil, QueryError(fmt.Sprintf("unknown group key %q", g))
		}
		st.cols = append(st.cols, col)
	}
	var err error
	if st.where, st.args, err = candidates(spec.Filter); err != nil {
		return nil, err
	}
	one := spec.Filter
	one.Under = 0
	if st.oneWhere, st.oneArgs, err = candidates(one); err != nil {
		return nil, err
	}
	if err := ix.rankGroups(ctx, st); err != nil {
		return nil, err
	}
	if ix.queues == nil {
		ix.queues = make(map[string]*queueState)
	}
	if len(ix.queues) >= maxCachedQueues {
		oldest := ""
		for k, other := range ix.queues {
			if oldest == "" || other.used < ix.queues[oldest].used {
				oldest = k
			}
		}
		delete(ix.queues, oldest)
	}
	ix.queues[key] = st
	return st, nil
}

// Queue returns the next unreviewed items for a review queue.
func (ix *Index) Queue(ctx context.Context, spec QueueSpec) (QueueResult, error) {
	res := QueueResult{Groups: []QueueGroup{}, Items: []Row{}}
	if _, err := spec.Order.sql(); err != nil {
		return res, err
	}
	if spec.Limit <= 0 {
		spec.Limit = 20
	}

	ix.qmu.Lock()
	defer ix.qmu.Unlock()
	st, err := ix.queueFor(ctx, spec)
	if err != nil {
		return res, err
	}
	if err := ix.fillCounts(ctx, st); err != nil {
		return res, err
	}

	res.GroupTotal, res.Total, res.TotalSize = len(st.groups), st.total, st.size
	pass := int64(max(spec.Offset, 0))
	current := -1
	for i := range st.groups {
		g := &st.groups[i]
		remaining := g.Total - st.done[g.key()] - st.cov[g.key()]
		if remaining <= 0 {
			continue
		}
		res.Remaining += remaining
		res.GroupCount++
		if current < 0 {
			if pass >= remaining {
				pass -= remaining
				continue
			}
			current, res.GroupPosition = i, i+1
		}
		if len(res.Groups) < maxGroupsReturned {
			if err := ix.labelGroup(ctx, spec.Groups, g); err != nil {
				return res, err
			}
			out := *g
			out.Remaining = remaining
			res.Groups = append(res.Groups, out)
		}
	}
	if current < 0 {
		return res, nil
	}
	res.Items, err = ix.nextItems(ctx, st, &st.groups[current], spec.Order, int(pass), spec.Limit)
	if err != nil {
		return res, err
	}
	return res, ix.FillPaths(ctx, res.Items)
}

// rankGroups finds every group of a queue with its totals, in serving order.
// Groups are ranked by everything in them, reviewed or not, so the order does
// not shift under the user as they work, and a share keeps its place after
// its biggest group is finished.
func (ix *Index) rankGroups(ctx context.Context, st *queueState) error {
	q := `SELECT COUNT(*), COALESCE(SUM(e.size), 0)`
	if len(st.cols) > 0 {
		q += ", " + strings.Join(st.cols, ", ")
	}
	q += ` FROM entries e` + rankReach(st.spec) + st.where
	if len(st.cols) > 0 {
		q += " GROUP BY " + strings.Join(st.cols, ", ") + fmt.Sprintf(" ORDER BY 2 DESC LIMIT %d", maxGroupsScanned)
	}
	rows, err := ix.db.QueryContext(ctx, q, st.args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		g := QueueGroup{raw: make([]any, len(st.cols))}
		dest := []any{&g.Total, &g.Size}
		for i := range g.raw {
			dest = append(dest, &g.raw[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		if g.Total > 0 {
			st.groups = append(st.groups, g)
			st.total += g.Total
			st.size += g.Size
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sortGroups(st.groups, st.spec.Groups)
	return nil
}

// fillCounts counts whatever the state does not hold. In the course of a
// review that is nothing: the counts follow each annotation as it is made.
func (ix *Index) fillCounts(ctx context.Context, st *queueState) error {
	sel, groupBy := "", ""
	if len(st.cols) > 0 {
		sel, groupBy = ", "+strings.Join(st.cols, ", "), " GROUP BY "+strings.Join(st.cols, ", ")
	}
	var err error
	if st.done == nil {
		// CROSS JOIN pins the join order: start from the annotations, which
		// are few beside the entries.
		q := `SELECT COUNT(*)` + sel + ` FROM annot a CROSS JOIN entries e ON e.id = a.entry` + st.where + groupBy
		if st.done, err = ix.countByGroup(ctx, len(st.cols), q, st.args); err != nil {
			return err
		}
	}
	if st.spec.ExcludeCovered && st.cov == nil {
		var dirs int64
		if err := ix.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM covered`).Scan(&dirs); err != nil {
			return err
		}
		var q string
		switch {
		case dirs == 0:
			st.cov = map[string]int64{}
			return nil
		case dirs*4 < ix.Info.Dirs:
			// Few folders are covered: visit what is in them.
			q = `SELECT COUNT(*)` + sel + ` FROM covered c CROSS JOIN entries e ON e.parent = c.dir
				LEFT JOIN annot a ON a.entry = e.id` + st.where + ` AND a.entry IS NULL` + groupBy
		default:
			// Much of the index is covered: one pass over the candidates
			// costs less than reaching each covered item through its folder.
			q = `SELECT COUNT(*)` + sel + rowFrom + st.where + ` AND a.entry IS NULL AND ` + coveredSQL + groupBy
		}
		if st.cov, err = ix.countByGroup(ctx, len(st.cols), q, st.args); err != nil {
			return err
		}
	}
	return nil
}

// countByGroup runs a query that yields a count followed by the group columns.
func (ix *Index) countByGroup(ctx context.Context, depth int, q string, args []any) (map[string]int64, error) {
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64)
	for rows.Next() {
		var n int64
		raw := make([]any, depth)
		dest := []any{&n}
		for i := range raw {
			dest = append(dest, &raw[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		counts[lastKey(raw)] = n
	}
	return counts, rows.Err()
}

// groupOf reports which group of a queue an entry falls in, if it is one of
// the queue's candidates at all, and whether it sits in a covered folder.
func (ix *Index) groupOf(ctx context.Context, st *queueState, entry int64) (key string, covered, ok bool, err error) {
	q := `SELECT e.parent, ` + coveredSQL
	if len(st.cols) > 0 {
		q += ", " + strings.Join(st.cols, ", ")
	}
	q += ` FROM entries e` + st.oneWhere + ` AND e.id = ?`
	var parent int64
	raw := make([]any, len(st.cols))
	dest := []any{&parent, &covered}
	for i := range raw {
		dest = append(dest, &raw[i])
	}
	err = ix.db.QueryRowContext(ctx, q, append(slices.Clone(st.oneArgs), entry)...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	if under := st.spec.Filter.Under; under != 0 {
		for id := parent; id != under; {
			if id == 0 {
				return "", false, false, nil
			}
			if err := ix.db.QueryRowContext(ctx, `SELECT parent FROM entries WHERE id = ?`, id).Scan(&id); err != nil {
				return "", false, false, err
			}
		}
	}
	return lastKey(raw), covered, true, nil
}

// reach says how SQLite should get at the items of one group of total items:
// through a named index, by reading the whole table, or ("") as it sees fit.
//
// Its planner knows how large a group is on average, not how large this one
// is, and it trusts the size index too far: asked for the largest files of a
// small group, it walks every file on the pool from the largest down, a
// second or more per million entries. Reading the table in order costs a
// tenth of that whatever is asked, so that is the fallback wherever no index
// leads straight to the group.
func (ix *Index) reach(st *queueState, total int64, bySize bool) string {
	keys := st.spec.Groups
	if st.spec.Filter.Under != 0 || slices.Contains(keys, "folder") {
		return "" // a few folders' worth, found through the parent index
	}
	all := ix.Info.Files + ix.Info.Dirs
	switch {
	case len(keys) == 0:
		// Largest first with little left out: the size index is that order.
		if bySize && st.total*20 >= all {
			return ""
		}
	case slices.Contains(keys, "share") && inGroupIndex(keys):
		// The group index keeps one share's items of one type together,
		// largest first. Worth it for exactly that order, or for a group
		// small enough that fetching its rows one by one beats a full read.
		if (bySize && slices.Contains(keys, "type")) || total*5 < all {
			return " INDEXED BY entries_group"
		}
	}
	return " NOT INDEXED"
}

// inGroupIndex reports whether every group key is a column of entries_group.
func inGroupIndex(keys []string) bool {
	return !slices.ContainsFunc(keys, func(k string) bool { return k != "share" && k != "type" })
}

// rankReach is reach for the query that totals every group. The group index
// answers it without touching the table when it holds every column involved;
// otherwise one read of the table beats a million fetches through an index.
func rankReach(spec QueueSpec) string {
	f := spec.Filter
	inIndex := inGroupIndex(spec.Groups) && len(f.Exts) == 0 && f.ModifiedBefore == 0 && f.Name == ""
	if f.Under != 0 || (inIndex && f.Kind != "") {
		return ""
	}
	return " NOT INDEXED"
}

// pendingFrom returns the FROM and WHERE clauses for the unreviewed items of
// one group.
func pendingFrom(st *queueState, raw []any, reach string) (string, []any) {
	q := ` FROM entries e` + reach + ` LEFT JOIN annot a ON a.entry = e.id` + st.where + " AND a.entry IS NULL"
	if st.spec.ExcludeCovered {
		q += " AND NOT " + coveredSQL
	}
	args := slices.Clone(st.args)
	for i, col := range st.cols {
		q += " AND " + col + " = ?"
		args = append(args, raw[i])
	}
	return q, args
}

// nextItems returns unreviewed items of a group, in order, passing over the
// first offset of them.
func (ix *Index) nextItems(ctx context.Context, st *queueState, g *QueueGroup, by Sort, offset, limit int) ([]Row, error) {
	order, err := by.sql()
	if err != nil {
		return nil, err
	}
	from, args := pendingFrom(st, g.raw, ix.reach(st, g.Total, by.Key == "" || by.Key == "size"))
	need := offset + limit
	if need > headSize {
		// Deeper than is held ready: page through the index itself.
		return ix.query(ctx, `SELECT `+rowCols+from+order+limitSQL(limit, offset), args...)
	}
	for {
		h := st.head
		fresh := h == nil || h.group != g.key() || h.order != order
		if fresh {
			ids, err := ix.ids(ctx, `SELECT e.id`+from+order+limitSQL(headSize+1, 0), args...)
			if err != nil {
				return nil, err
			}
			h = &queueHead{group: g.key(), order: order, ids: ids, full: len(ids) <= headSize}
			st.head = h
		} else if err := ix.dropReviewed(ctx, h, need); err != nil {
			return nil, err
		}
		if fresh || h.full || len(h.ids) >= need {
			return ix.Rows(ctx, h.ids[min(offset, len(h.ids)):min(need, len(h.ids))])
		}
		st.head = nil // run dry: fetch the next stretch
	}
}

// dropReviewed removes from the head of a held list the items reviewed since
// it was fetched, checking no further than is needed to serve need items.
// Nothing returns to the list this way; an item that comes back into review
// discards the list instead.
func (ix *Index) dropReviewed(ctx context.Context, h *queueHead, need int) error {
	for checked := 0; checked < need && checked < len(h.ids); {
		end := min(len(h.ids), checked+max(need-checked, 64))
		chunk := h.ids[checked:end]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		reviewed, err := ix.ids(ctx, `SELECT entry FROM annot WHERE entry IN (`+marks+`)`, args...)
		if err != nil {
			return err
		}
		if len(reviewed) == 0 {
			checked = end
			continue
		}
		kept := h.ids[:checked]
		for _, id := range chunk {
			if !slices.Contains(reviewed, id) {
				kept = append(kept, id)
			}
		}
		checked = len(kept)
		h.ids = append(kept, h.ids[end:]...)
	}
	return nil
}

func (ix *Index) ids(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Rows returns the entries with the given IDs, in the order given.
func (ix *Index) Rows(ctx context.Context, ids []int64) ([]Row, error) {
	if len(ids) == 0 {
		return []Row{}, nil
	}
	args := make([]any, len(ids))
	place := make(map[int64]int, len(ids))
	for i, id := range ids {
		args[i], place[id] = id, i
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := ix.query(ctx, `SELECT `+rowCols+rowFrom+` WHERE e.id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return place[rows[i].ID] < place[rows[j].ID] })
	return rows, nil
}

// PendingInGroup returns the IDs of every unreviewed item of one group of a
// queue, in no particular order. group holds one value for each of the
// queue's group keys, as GroupValue.Value gives them.
func (ix *Index) PendingInGroup(ctx context.Context, spec QueueSpec, group []any) ([]int64, error) {
	raw, err := ix.groupRaw(spec.Groups, group)
	if err != nil {
		return nil, err
	}
	ix.qmu.Lock()
	st, err := ix.queueFor(ctx, spec)
	ix.qmu.Unlock()
	if err != nil {
		return nil, err
	}
	var total int64
	key := lastKey(raw)
	for i := range st.groups {
		if st.groups[i].key() == key {
			total = st.groups[i].Total
			break
		}
	}
	from, args := pendingFrom(st, raw, ix.reach(st, total, false))
	return ix.ids(ctx, `SELECT e.id`+from, args...)
}

// groupRaw turns the values that name a group in the API back into what the
// index stores for each group key.
func (ix *Index) groupRaw(keys []string, values []any) ([]any, error) {
	if len(values) != len(keys) {
		return nil, QueryError("a group is named by one value for each group key")
	}
	raw := make([]any, len(keys))
	for i, key := range keys {
		bad := QueryError(fmt.Sprintf("%v does not name a %s group", values[i], key))
		switch key {
		case "share", "folder":
			var id int64
			switch v := values[i].(type) {
			case int64:
				id = v
			case int:
				id = int64(v)
			case float64: // a number that arrived as JSON
				if id = int64(v); float64(id) != v {
					return nil, bad
				}
			default:
				return nil, bad
			}
			raw[i] = id
		case "type":
			name, _ := values[i].(string)
			cat, ok := CatByName(name)
			if !ok {
				return nil, bad
			}
			raw[i] = int64(cat)
		case "ext":
			ext, ok := values[i].(string)
			if !ok {
				return nil, bad
			}
			raw[i] = ext
		case "age":
			name, _ := values[i].(string)
			n := slices.IndexFunc(ageBuckets, func(b ageBucket) bool { return b.name == name })
			if n < 0 {
				return nil, bad
			}
			raw[i] = int64(n)
		default:
			return nil, QueryError(fmt.Sprintf("unknown group key %q", key))
		}
	}
	return raw, nil
}

// groupKeys returns, for each depth of grouping, a string identifying the
// group's ancestor at that depth.
func groupKeys(raw []any) []string {
	keys := make([]string, len(raw))
	key := ""
	for d, v := range raw {
		key += fmt.Sprintf("%v\x00", v)
		keys[d] = key
	}
	return keys
}

func lastKey(raw []any) string {
	if len(raw) == 0 {
		return ""
	}
	return groupKeys(raw)[len(raw)-1]
}

// sortGroups orders groups so that, at every level of grouping, the group
// holding the most bytes comes first: the biggest share, then within it the
// biggest file type, and so on. Age is the exception. Its groups run oldest
// first, since the point of grouping by age is to reach the oldest things.
func sortGroups(groups []QueueGroup, keys []string) {
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
		g.keys = groupKeys(g.raw)
		for d, key := range g.keys {
			totals[d][key] += g.Size
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := &groups[i], &groups[j]
		for d := range depth {
			if a.keys[d] == b.keys[d] {
				continue
			}
			if keys[d] == "age" {
				x, _ := a.raw[d].(int64)
				y, _ := b.raw[d].(int64)
				return x < y
			}
			if ta, tb := totals[d][a.keys[d]], totals[d][b.keys[d]]; ta != tb {
				return ta > tb
			}
			return a.keys[d] < b.keys[d]
		}
		return false
	})
}

// labelGroup fills in a group's values the first time the group is shown.
func (ix *Index) labelGroup(ctx context.Context, keys []string, g *QueueGroup) error {
	if g.Values != nil {
		return nil
	}
	values := make([]GroupValue, len(keys))
	for i, key := range keys {
		v := GroupValue{Key: key}
		switch key {
		case "share":
			id, _ := g.raw[i].(int64)
			share, err := ix.Entry(ctx, id)
			if err != nil {
				return err
			}
			v.Value, v.Label = id, share.Name
		case "type":
			cat, _ := g.raw[i].(int64)
			v.Label = CatName(int(cat))
			v.Value = v.Label
		case "ext":
			v.Label, _ = g.raw[i].(string)
			v.Value = v.Label
		case "folder":
			id, _ := g.raw[i].(int64)
			path, err := ix.EntryPath(ctx, id)
			if err != nil {
				return err
			}
			v.Value, v.Label = id, path
		case "age":
			n, _ := g.raw[i].(int64)
			v.Value, v.Label = ageBuckets[n].name, ageBuckets[n].label
		}
		values[i] = v
	}
	g.Values = values
	return nil
}
