package refdata

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func TestVatRateIDForPicksNewestWindowNotAfterDate(t *testing.T) {
	s := &Snapshot{vatRatesByTypeCountry: map[string][]vatRateWindow{
		"pickup|SI": { // sorted ValidFrom desc, as Load builds it
			{ValidFrom: "2026-01-01", RateID: 3},
			{ValidFrom: "2020-01-01", RateID: 2},
			{ValidFrom: "2010-01-01", RateID: 1},
		},
	}}
	cases := []struct {
		date   string
		wantID int
		wantOK bool
	}{
		{"2026-07-10", 3, true},
		{"2026-01-01", 3, true}, // boundary: valid_from <= date
		{"2025-12-31", 2, true},
		{"2015-06-01", 1, true}, // 2020 window is in the future for this date; falls to 2010
		{"2009-12-31", 0, false}, // before every window
	}
	for _, c := range cases {
		id, ok := s.VatRateIDFor("pickup", "SI", c.date)
		if ok != c.wantOK || (ok && id != c.wantID) {
			t.Errorf("date %s: got (%d,%v)", c.date, id, ok)
		}
	}
	if _, ok := s.VatRateIDFor("pickup", "XX", "2026-07-10"); ok {
		t.Error("unknown country must miss")
	}
}

// TestSnapshotLookupsMatchSQL is the Phase-1 equivalence gate: for every (vat_type, country)
// pair present in the data, the snapshot answer must equal the php DAO's SQL executed live.
// Gated on GO_BA_TEST_MYSQL_DSN (go-pe's env-gated integration-test pattern).
func TestSnapshotLookupsMatchSQL(t *testing.T) {
	dsn := os.Getenv("GO_BA_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GO_BA_TEST_MYSQL_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap, err := Load(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}

	const date = "2026-07-10"
	rows, err := db.Query(`SELECT DISTINCT vat_type, vat_country_code FROM vat_rate`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var vatType, cc string
		if err := rows.Scan(&vatType, &cc); err != nil {
			t.Fatal(err)
		}
		// the php DAO's exact SQL (VatRateDao::FIND_ONE_ID_BY_VAT_TYPE_AND_VAT_COUNTRY)
		var sqlID sql.NullInt64
		err := db.QueryRow(`SELECT vr.id FROM vat_rate vr
			JOIN vat_rate_dates vrd ON vr.id = vrd.vat_rate_id
			WHERE vr.vat_type = ? AND vr.vat_country_code = ? AND vrd.valid_from <= ?
			ORDER BY vrd.valid_from DESC LIMIT 1`, vatType, cc, date).Scan(&sqlID)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		gotID, gotOK := snap.VatRateIDFor(vatType, cc, date)
		if gotOK != sqlID.Valid || (gotOK && int64(gotID) != sqlID.Int64) {
			t.Errorf("%s/%s: snapshot=(%d,%v) sql=(%v)", vatType, cc, gotID, gotOK, sqlID)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no vat pairs checked")
	}
	t.Logf("vat pairs checked: %d", checked)

	// holidays: per-country active counts must match the php DAO's unbounded query
	for _, countryID := range []int{191, 110, 56, 16, 23} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM non_working_days WHERE active = 1 AND pickup_country_id = ?`,
			countryID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if got := len(snap.HolidaysByCountry[countryID]); got != n {
			t.Errorf("country %d: snapshot holidays %d, sql %d", countryID, got, n)
		}
	}
}
