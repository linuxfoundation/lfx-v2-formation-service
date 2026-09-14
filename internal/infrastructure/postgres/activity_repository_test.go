// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/uptrace/bun"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
)

// The fixture is deliberately larger than a page and larger than the planner's
// seq-scan threshold. Both matter: a filter tested against a handful of rows
// returns the right entries by accident, and an index assertion against a
// small table asserts nothing, because Postgres correctly prefers a sequential
// scan over one.
const (
	activityFixtureFormations = 4
	activityFixtureItems      = 30
	activityFixtureEntries    = 3000
)

// activityFixture is one formation's worth of seeded feed, plus the two items
// the filter tests target.
type activityFixture struct {
	db *bun.DB

	// plans captures the SQL the repository actually issues, for the tests
	// that assert a query plan rather than a result.
	plans *queryCapture

	formationUID uuid.UUID

	// targetUID's only entry is the oldest in the whole feed. That placement
	// is the point of the fixture rather than a detail of it: this is exactly
	// the item whose history renders empty against an unfiltered read, because
	// the consumer gives up paging before reaching it.
	targetUID  uuid.UUID
	targetULID string

	// pagedUID has more history than a page holds, so a filtered read of it
	// has to page. A filtered read usually returns fewer entries than a page,
	// which is why the multi-page path is the one that goes unexercised.
	pagedUID   uuid.UUID
	pagedULIDs []string

	// otherFormationUID carries its own feed, so a filtered read can be shown
	// to keep the formation predicate rather than trading it for the item one.
	otherFormationUID uuid.UUID
	otherFormationLen int
}

// seedActivityFixture stands up several formations, items on each and a few
// thousand entries, with the two targeted items placed as described on
// activityFixture.
func seedActivityFixture(t *testing.T, db *bun.DB) *activityFixture {
	t.Helper()
	ctx := context.Background()

	template, err := NewTemplateRepo(db).Upsert(ctx, &model.Template{
		Name:     "activity-filter-template",
		Version:  1,
		State:    model.TemplatePublished,
		Priority: 100,
		Match:    "always",
		Sections: []model.TemplateSection{},
	})
	if err != nil {
		t.Fatalf("seed template: %v", err)
	}

	formationRepo := NewFormationRepo(db)
	itemRepo := NewItemRepo(db)

	formations := make([]uuid.UUID, 0, activityFixtureFormations)
	itemsByFormation := make(map[uuid.UUID][]uuid.UUID, activityFixtureFormations)
	for f := range activityFixtureFormations {
		formation, err := formationRepo.Create(ctx, &model.Formation{
			ProjectUID:      fmt.Sprintf("activity-filter-project-%d", f),
			TemplateUID:     template.UID,
			TemplateVersion: template.Version,
		})
		if err != nil {
			t.Fatalf("seed formation %d: %v", f, err)
		}
		formations = append(formations, formation.UID)

		items := make([]*model.Item, 0, activityFixtureItems)
		for i := range activityFixtureItems {
			items = append(items, &model.Item{
				FormationUID: formation.UID,
				ItemKey:      fmt.Sprintf("item_%d", i),
				SectionKey:   "legal_and_entity",
				Position:     i,
				Title:        fmt.Sprintf("Item %d", i),
				Status:       model.StatusNotStarted,
			})
		}
		if _, err := itemRepo.InsertMany(ctx, items); err != nil {
			t.Fatalf("seed items for formation %d: %v", f, err)
		}
		uids := make([]uuid.UUID, 0, len(items))
		for _, item := range items {
			uids = append(uids, item.UID)
		}
		itemsByFormation[formation.UID] = uids
	}

	fixture := &activityFixture{
		db:                db,
		formationUID:      formations[0],
		targetUID:         itemsByFormation[formations[0]][0],
		pagedUID:          itemsByFormation[formations[0]][1],
		otherFormationUID: formations[1],
	}

	// Monotonically increasing ULIDs by construction. ulid.Make() reads the
	// clock, so several thousand in a tight loop share a millisecond and order
	// only by random entropy — which would make "oldest" and "newest first"
	// unassertable.
	entries := make([]*model.ActivityEntry, 0, activityFixtureEntries)
	newEntry := func(i int, formationUID uuid.UUID, itemUID *uuid.UUID) *model.ActivityEntry {
		id := ulid.MustNew(uint64(i+1), ulid.DefaultEntropy()).String()
		return &model.ActivityEntry{
			ULID:         id,
			FormationUID: formationUID,
			ItemUID:      itemUID,
			Actor:        fmt.Sprintf("actor-%d", i),
			SetBy:        model.SetByUser,
			Action:       "status_changed",
		}
	}

	// Oldest entry in the feed, and the only one the target item has.
	first := newEntry(0, fixture.formationUID, &fixture.targetUID)
	fixture.targetULID = first.ULID
	entries = append(entries, first)

	for i := 1; i < activityFixtureEntries; i++ {
		formationUID := formations[i%len(formations)]
		itemUIDs := itemsByFormation[formationUID]

		var itemUID *uuid.UUID
		switch {
		case formationUID == fixture.formationUID && i%7 == 0:
			// Give the paged item enough history to span several pages.
			itemUID = &fixture.pagedUID
		case i%11 == 0:
			// A formation-level entry: template expansion or upgrade, which
			// concerns no single item. A filtered read must never return one.
			itemUID = nil
		default:
			candidate := itemUIDs[i%len(itemUIDs)]
			if formationUID == fixture.formationUID && candidate == fixture.targetUID {
				candidate = itemUIDs[2]
			}
			itemUID = &candidate
		}

		entry := newEntry(i, formationUID, itemUID)
		if itemUID != nil && *itemUID == fixture.pagedUID {
			fixture.pagedULIDs = append(fixture.pagedULIDs, entry.ULID)
		}
		if formationUID == fixture.otherFormationUID {
			fixture.otherFormationLen++
		}
		entries = append(entries, entry)
	}

	if _, err := db.NewInsert().Model(&entries).Exec(ctx); err != nil {
		t.Fatalf("seed activity entries: %v", err)
	}

	// The planner needs statistics before an index assertion means anything:
	// against a table it believes is tiny, a sequential scan is the correct
	// choice and the gate would fail for the wrong reason.
	if _, err := db.ExecContext(ctx, "ANALYZE formation_activity"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	if len(fixture.pagedULIDs) <= maxActivityLimit {
		t.Fatalf("paged item has %d entries, want more than one full page (%d)", len(fixture.pagedULIDs), maxActivityLimit)
	}

	// Attached after seeding so the capture holds only what a test provoked.
	fixture.plans = &queryCapture{}
	db.AddQueryHook(fixture.plans)
	return fixture
}

// The target item's only entry is the oldest in
// a feed spanning many pages, so an unfiltered reader has to walk the whole
// thing to find it. The filtered read answers in one request.
func TestListFilteredReturnsTheItemsHistoryInOneRequest(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fixture := seedActivityFixture(t, db)
	repo := NewActivityRepo(db)

	// Sanity, and the whole reason the feature exists: the first page of the
	// unfiltered feed does not contain the entry. Without this the assertion
	// below could pass against no filter at all.
	unfiltered, _, err := repo.List(ctx, fixture.formationUID, nil, "", maxActivityLimit)
	if err != nil {
		t.Fatalf("unfiltered list: %v", err)
	}
	for _, e := range unfiltered {
		if e.ULID == fixture.targetULID {
			t.Fatal("the target entry is on the unfiltered first page, so this fixture proves nothing")
		}
	}

	entries, nextCursor, err := repo.List(ctx, fixture.formationUID, &fixture.targetUID, "", maxActivityLimit)
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}

	// One request, and no second one available to make: requests fall from as
	// many as it takes to walk the feed, to exactly one, with nothing fetched
	// only to be discarded.
	if len(entries) != 1 {
		t.Fatalf("filtered entries: got %d, want 1", len(entries))
	}
	if nextCursor != "" {
		t.Errorf("next_cursor: got %q, want empty — the item's history fits in one page", nextCursor)
	}
	if entries[0].ULID != fixture.targetULID {
		t.Errorf("entry ulid: got %q, want the item's only entry %q", entries[0].ULID, fixture.targetULID)
	}
	assertEveryEntryBelongsTo(t, entries, fixture.targetUID)
}

// Nothing belonging to another item, and nothing belonging to
// no item, comes back from a filtered read.
func TestListFilteredExcludesEveryOtherItem(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fixture := seedActivityFixture(t, db)
	repo := NewActivityRepo(db)

	entries, _, err := repo.List(ctx, fixture.formationUID, &fixture.pagedUID, "", maxActivityLimit)
	if err != nil {
		t.Fatalf("filtered list: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("filtered list returned nothing, so exclusion is untested")
	}
	assertEveryEntryBelongsTo(t, entries, fixture.pagedUID)
}

// A filtered read pages by the same mechanism as an unfiltered
// one, over the item's own sequence. Seeded with more history than a page
// holds, because a filtered read usually returns fewer entries than a page and
// the multi-page path is the one that breaks unnoticed.
func TestListFilteredPagesAcrossTheItemsOwnSequence(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fixture := seedActivityFixture(t, db)
	repo := NewActivityRepo(db)

	const pageSize = 10
	var seen []string
	cursor := ""
	pages := 0
	for {
		entries, next, err := repo.List(ctx, fixture.formationUID, &fixture.pagedUID, cursor, pageSize)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		assertEveryEntryBelongsTo(t, entries, fixture.pagedUID)
		for _, e := range entries {
			seen = append(seen, e.ULID)
		}
		if next == "" {
			break
		}
		cursor = next
		if pages > len(fixture.pagedULIDs) {
			t.Fatal("paging did not terminate — the cursor is not advancing")
		}
	}

	if pages < 2 {
		t.Fatalf("pages walked: got %d, want more than one — the fixture is not exercising filtered paging", pages)
	}

	// Newest first over the item's own history, every entry exactly once.
	want := make([]string, len(fixture.pagedULIDs))
	for i, id := range fixture.pagedULIDs {
		want[len(fixture.pagedULIDs)-1-i] = id
	}
	if len(seen) != len(want) {
		t.Fatalf("entries walked: got %d, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("entry %d: got %q, want %q — filtered paging is not newest-first over the item's sequence", i, seen[i], want[i])
		}
	}
}

// The formation predicate stays in the query alongside the item one.
// This is the feature's one real authorization hazard at the repository level:
// a well-formed item UID from another formation must select nothing, not that
// item's history.
func TestListFilteredKeepsTheFormationPredicate(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fixture := seedActivityFixture(t, db)
	repo := NewActivityRepo(db)

	// An item that really does have history, asked for under the wrong
	// formation. Reading it back under its own formation first means a later
	// empty result is the predicate holding, not an item with no entries.
	own, _, err := repo.List(ctx, fixture.formationUID, &fixture.pagedUID, "", maxActivityLimit)
	if err != nil {
		t.Fatalf("list under its own formation: %v", err)
	}
	if len(own) == 0 {
		t.Fatal("the item has no history, so the cross-formation assertion below proves nothing")
	}

	entries, nextCursor, err := repo.List(ctx, fixture.otherFormationUID, &fixture.pagedUID, "", maxActivityLimit)
	if err != nil {
		t.Fatalf("cross-formation list: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cross-formation entries: got %d, want 0 — the formation predicate was traded for the item one", len(entries))
	}
	if nextCursor != "" {
		t.Errorf("cross-formation next_cursor: got %q, want empty", nextCursor)
	}
}

// The unfiltered feed is undisturbed: paged to exhaustion it
// returns every entry the formation has, newest first, including the ones that
// concern no item at all.
func TestListUnfilteredFeedIsUnchangedByTheFilter(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fixture := seedActivityFixture(t, db)
	repo := NewActivityRepo(db)

	// The baseline is read from the table directly rather than through the
	// method under test, so "unchanged" is measured against the rows rather
	// than against the same code path.
	var baseline []string
	if err := db.NewSelect().
		Table("formation_activity").
		Column("ulid").
		Where("formation_uid = ?", fixture.formationUID).
		OrderExpr("ulid DESC").
		Scan(ctx, &baseline); err != nil {
		t.Fatalf("baseline read: %v", err)
	}

	var seen []string
	withoutItem := 0
	cursor := ""
	for page := 0; page <= len(baseline); page++ {
		entries, next, err := repo.List(ctx, fixture.formationUID, nil, cursor, maxActivityLimit)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, e := range entries {
			seen = append(seen, e.ULID)
			if e.ItemUID == nil {
				withoutItem++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	if len(seen) != len(baseline) {
		t.Fatalf("entries walked: got %d, want %d", len(seen), len(baseline))
	}
	for i := range baseline {
		if seen[i] != baseline[i] {
			t.Fatalf("entry %d: got %q, want %q — unfiltered order or paging moved", i, seen[i], baseline[i])
		}
	}
	if withoutItem == 0 {
		t.Error("no entry without an item_uid came back, so the unfiltered read is not covering formation-level entries")
	}
}

// Adding an index can change an unrelated plan choice, and that is
// silent if nobody asserts it.
//
// Asserted as plan equality across the index's absence and presence, rather
// than as "the unfiltered read uses formation_activity_feed_idx". The latter
// looks like the stronger check and is in fact untrue independently of this
// feature: on a feed shared by several formations the planner walks the ULID
// primary key backwards under a LIMIT instead, because the formation predicate
// is not selective enough to pay for the composite index. That choice is the
// planner's and it moves with the data, so pinning it would make this test a
// report on row distribution. What this feature is answerable for is narrower
// and is exactly what is checked here: it did not change the plan.
func TestNewIndexDoesNotChangeTheUnfilteredReadsPlan(t *testing.T) {
	db := testDB(t)
	fixture := seedActivityFixture(t, db)

	dropItemIndex(t, fixture)
	before := explainList(t, fixture, nil, "")

	restoreItemIndex(t, fixture)
	after := explainList(t, fixture, nil, "")

	if before != after {
		t.Errorf("the new index changed the unfiltered read's plan.\nwithout it:\n%s\n\nwith it:\n%s", before, after)
	}
}

// The cost gate. A test that only asserts correct rows
// passes just as happily against a formation-wide scan, which is the silent
// regression this exists to catch: the consumer's page-and-discard relocated
// into Postgres, where nobody is measuring it.
//
// Verified to be a real gate rather than a passing test by dropping the index
// and watching it fail; see the note in docs/activity-contract.md.
func TestFilteredReadUsesTheItemIndexAndNeverScansTheFeed(t *testing.T) {
	db := testDB(t)
	fixture := seedActivityFixture(t, db)

	// Both the first page and a subsequent one. The cursor adds `ulid <` to
	// the same query, and the index's trailing ulid DESC column is what keeps
	// that served by the index rather than by a filter-and-sort. Asserting
	// only the first page would leave the paged path — the one a consumer
	// reaches on any item with real history — free to regress unnoticed.
	cursors := map[string]string{
		"first page": "",
		"paged":      fixture.pagedULIDs[len(fixture.pagedULIDs)/2],
	}
	for name, cursor := range cursors {
		t.Run(name, func(t *testing.T) {
			plan := explainList(t, fixture, &fixture.pagedUID, cursor)
			if !strings.Contains(plan, "formation_activity_item_idx") {
				t.Errorf("filtered read does not use formation_activity_item_idx:\n%s", plan)
			}
			if strings.Contains(plan, "Seq Scan on formation_activity") {
				t.Errorf("filtered read sequentially scans formation_activity:\n%s", plan)
			}
		})
	}
}

// assertEveryEntryBelongsTo is the assertion that makes a filtered read's
// result meaningful: the right count with a wrong row in it is still wrong,
// and an entry with no item is never part of a filtered answer.
func assertEveryEntryBelongsTo(t *testing.T, entries []*model.ActivityEntry, itemUID uuid.UUID) {
	t.Helper()
	for _, e := range entries {
		if e.ItemUID == nil {
			t.Errorf("entry %s has no item_uid, so it cannot belong to %s", e.ULID, itemUID)
			continue
		}
		if *e.ItemUID != itemUID {
			t.Errorf("entry %s belongs to item %s, want %s", e.ULID, *e.ItemUID, itemUID)
		}
	}
}

// explainList returns the query plan for the statement List actually issues.
//
// The SQL is captured from the driver rather than rebuilt here. Rebuilding it
// would assert the plan of a query that resembles the real one, which is the
// failure mode an index gate cannot afford: the gate would keep passing after
// List itself stopped matching it.
func explainList(t *testing.T, f *activityFixture, itemUID *uuid.UUID, cursor string) string {
	t.Helper()
	ctx := context.Background()

	f.plans.reset()
	if _, _, err := NewActivityRepo(f.db).List(ctx, f.formationUID, itemUID, cursor, maxActivityLimit); err != nil {
		t.Fatalf("list for plan capture: %v", err)
	}
	query := f.plans.last()
	if query == "" {
		t.Fatal("no query captured from List")
	}

	var lines []string
	if err := f.db.NewRaw("EXPLAIN "+query).Scan(ctx, &lines); err != nil {
		t.Fatalf("explain %q: %v", query, err)
	}
	return strings.Join(lines, "\n")
}

// dropItemIndex removes the item index and registers its restoration, so a
// t.Fatalf between the drop and the restore cannot leave it missing for every
// later test in the run — which would fail the cost gate and blame the index
// rather than this test.
func dropItemIndex(t *testing.T, f *activityFixture) {
	t.Helper()
	t.Cleanup(func() { restoreItemIndex(t, f) })
	if _, err := f.db.ExecContext(context.Background(),
		"DROP INDEX IF EXISTS formation_activity_item_idx"); err != nil {
		t.Fatalf("drop the item index: %v", err)
	}
}

// restoreItemIndex recreates the index from the definition embedded in
// schema.sql rather than from a copy written here. A copy would silently
// restore a stale definition after the column order in the schema changed,
// which is the one thing the cost gate cannot afford to be wrong about.
func restoreItemIndex(t *testing.T, f *activityFixture) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), itemIndexDDL(t)); err != nil {
		t.Fatalf("restore the item index: %v", err)
	}
}

func itemIndexDDL(t *testing.T) string {
	t.Helper()
	for line := range strings.SplitSeq(schemaSQL, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "CREATE INDEX") && strings.Contains(trimmed, "formation_activity_item_idx") {
			return strings.TrimSuffix(trimmed, ";")
		}
	}
	t.Fatal("no CREATE INDEX for formation_activity_item_idx in the embedded schema")
	return ""
}

// queryCapture records the SELECTs bun sends to the driver, arguments already
// interpolated, so one can be handed straight to EXPLAIN.
//
// One hook is attached per fixture and reset before each capture. bun offers
// no way to remove a hook, so attaching one per call would accumulate them on
// a shared handle for the life of the test.
type queryCapture struct {
	queries []string
}

func (c *queryCapture) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(event.Query)), "SELECT") {
		c.queries = append(c.queries, event.Query)
	}
	return ctx
}

func (c *queryCapture) AfterQuery(context.Context, *bun.QueryEvent) {}

func (c *queryCapture) reset() { c.queries = nil }

func (c *queryCapture) last() string {
	if len(c.queries) == 0 {
		return ""
	}
	return c.queries[len(c.queries)-1]
}
