package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func restNormalizeCell(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	default:
		return x
	}
}

func restScanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = restNormalizeCell(raw[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func restQueryTable(app *AppContext, table string, cols ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		sb := strings.Builder{}
		for i, col := range cols {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(col)
		}
		q := `SELECT ` + sb.String() + ` FROM ` + table + ` ORDER BY 1`
		rows, err := app.DB.Query(q)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		data, err := restScanRows(rows)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, data)
	}
}

func restListQuery(app *AppContext, query string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		rows, err := app.DB.Query(query)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		data, err := restScanRows(rows)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, data)
	}
}

func restPinFromParam(c *gin.Context) (string, error) {
	p := strings.TrimPrefix(c.Param("pinpath"), "/")
	return url.PathUnescape(p)
}

func restListPins(app *AppContext) gin.HandlerFunc {
	return restListQuery(app, `SELECT pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds FROM access_pins ORDER BY pin`)
}

func restGetPin(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		pin, err := restPinFromParam(c)
		if err != nil || strings.TrimSpace(pin) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pin path"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		row := app.DB.QueryRow(`SELECT pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds FROM access_pins WHERE pin = ?`, pin)
		var pinVal string
		var label sql.NullString
		var en, tmp int
		var exp sql.NullString
		var maxU sql.NullInt64
		var useC int64
		var hold sql.NullInt64
		if err := row.Scan(&pinVal, &label, &en, &tmp, &exp, &maxU, &useC, &hold); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "pin not found"})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		out := map[string]any{
			"pin": pinVal, "enabled": en, "temporary": tmp, "use_count": useC,
		}
		if label.Valid {
			out["label"] = label.String
		}
		if exp.Valid {
			out["expires_at"] = exp.String
		}
		if maxU.Valid {
			out["max_uses"] = maxU.Int64
		}
		if hold.Valid {
			out["door_hold_extra_seconds"] = hold.Int64
		}
		c.JSON(http.StatusOK, out)
	}
}

func restPostPin(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			PIN   string `json:"pin"`
			Label string `json:"label"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pin := strings.TrimSpace(body.PIN)
		if pin == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pin required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds) VALUES (?, ?, 1, 0, NULL, NULL, 0, NULL)`, pin, nullIfEmpty(body.Label))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostPinTemporary(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			PIN       string `json:"pin"`
			ExpiresAt string `json:"expires_at"`
			Label     string `json:"label"`
			MaxUses   *int64 `json:"max_uses"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pin := strings.TrimSpace(body.PIN)
		if pin == "" || strings.TrimSpace(body.ExpiresAt) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pin and expires_at (RFC3339) required"})
			return
		}
		if _, err := time.Parse(time.RFC3339, strings.TrimSpace(body.ExpiresAt)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("expires_at: %v", err)})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		var err error
		if body.MaxUses != nil && *body.MaxUses > 0 {
			_, err = app.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds) VALUES (?, ?, 1, 1, ?, ?, 0, NULL)`,
				pin, nullIfEmpty(body.Label), strings.TrimSpace(body.ExpiresAt), *body.MaxUses)
		} else {
			_, err = app.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds) VALUES (?, ?, 1, 1, ?, NULL, 0, NULL)`,
				pin, nullIfEmpty(body.Label), strings.TrimSpace(body.ExpiresAt))
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPatchPin(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		pin, err := restPinFromParam(c)
		if err != nil || strings.TrimSpace(pin) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pin path"})
			return
		}
		var body map[string]json.RawMessage
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		sets := []string{}
		args := []any{}
		if raw, ok := body["label"]; ok {
			var s string
			_ = json.Unmarshal(raw, &s)
			sets = append(sets, "label = ?")
			args = append(args, nullIfEmpty(strings.TrimSpace(s)))
		}
		if raw, ok := body["enabled"]; ok {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				v := 0
				if b {
					v = 1
				}
				sets = append(sets, "enabled = ?")
				args = append(args, v)
			}
		}
		if raw, ok := body["temporary"]; ok {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				v := 0
				if b {
					v = 1
				}
				sets = append(sets, "temporary = ?")
				args = append(args, v)
			}
		}
		if raw, ok := body["expires_at"]; ok {
			var s string
			_ = json.Unmarshal(raw, &s)
			s = strings.TrimSpace(s)
			if s == "" {
				sets = append(sets, "expires_at = NULL")
			} else {
				if _, err := time.Parse(time.RFC3339, s); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
				sets = append(sets, "expires_at = ?")
				args = append(args, s)
			}
		}
		if raw, ok := body["max_uses"]; ok {
			if string(raw) == "null" {
				sets = append(sets, "max_uses = NULL")
			} else {
				var n int64
				if json.Unmarshal(raw, &n) == nil {
					sets = append(sets, "max_uses = ?")
					args = append(args, n)
				}
			}
		}
		if raw, ok := body["door_hold_extra_seconds"]; ok {
			var n int
			if json.Unmarshal(raw, &n) == nil {
				if n < 0 || n > int((24*time.Hour)/time.Second) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "door_hold_extra_seconds out of range"})
					return
				}
				sets = append(sets, "door_hold_extra_seconds = ?")
				args = append(args, n)
			}
		}
		if len(sets) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no updatable fields"})
			return
		}
		args = append(args, pin)
		q := `UPDATE access_pins SET ` + strings.Join(sets, ", ") + ` WHERE pin = ?`
		res, err := app.DB.Exec(q, args...)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "pin not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeletePin(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		pin, err := restPinFromParam(c)
		if err != nil || strings.TrimSpace(pin) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pin path"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err = app.DB.Exec(`DELETE FROM access_pins WHERE pin = ?`, pin)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostTimeProfile(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			IANATZ      string `json:"iana_timezone"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id := strings.TrimSpace(body.ID)
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`
			INSERT INTO access_time_profiles (id, display_name, description, iana_timezone, respects_exception_calendar)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT(id) DO UPDATE SET
				display_name = excluded.display_name,
				description = excluded.description,
				iana_timezone = excluded.iana_timezone`,
			id, nullIfEmpty(body.DisplayName), nil, strings.TrimSpace(body.IANATZ))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPatchProfileRespectsExceptions(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			On bool `json:"on"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		v := 0
		if body.On {
			v = 1
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", id, "time profile"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`UPDATE access_time_profiles SET respects_exception_calendar = ? WHERE id = ?`, v, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteTimeProfile(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_time_profiles WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restListTimeWindows(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		pid := strings.TrimSpace(c.Query("time_profile_id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		var rows *sql.Rows
		var err error
		if pid != "" {
			rows, err = app.DB.Query(`SELECT id, time_profile_id, weekday, start_minute, end_minute FROM access_time_windows WHERE time_profile_id = ? ORDER BY id`, pid)
		} else {
			rows, err = app.DB.Query(`SELECT id, time_profile_id, weekday, start_minute, end_minute FROM access_time_windows ORDER BY time_profile_id, id`)
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		data, err := restScanRows(rows)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, data)
	}
}

func restPostTimeWindow(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			TimeProfileID string `json:"time_profile_id"`
			Weekday       int    `json:"weekday"`
			StartMinute   int    `json:"start_minute"`
			EndMinute     int    `json:"end_minute"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if body.Weekday < 0 || body.Weekday > 7 || body.StartMinute < 0 || body.StartMinute > 1439 || body.EndMinute < 0 || body.EndMinute > 1439 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "weekday 0-7, minutes 0-1439"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", strings.TrimSpace(body.TimeProfileID), "time profile"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		res, err := app.DB.Exec(`INSERT INTO access_time_windows (time_profile_id, weekday, start_minute, end_minute) VALUES (?, ?, ?, ?)`,
			strings.TrimSpace(body.TimeProfileID), body.Weekday, body.StartMinute, body.EndMinute)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, _ := res.LastInsertId()
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": id})
	}
}

func restPatchTimeWindow(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			Weekday       *int `json:"weekday"`
			StartMinute   *int `json:"start_minute"`
			EndMinute     *int `json:"end_minute"`
			TimeProfileID *string `json:"time_profile_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		sets := []string{}
		args := []any{}
		if body.Weekday != nil {
			if *body.Weekday < 0 || *body.Weekday > 7 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid weekday"})
				return
			}
			sets = append(sets, "weekday = ?")
			args = append(args, *body.Weekday)
		}
		if body.StartMinute != nil {
			sets = append(sets, "start_minute = ?")
			args = append(args, *body.StartMinute)
		}
		if body.EndMinute != nil {
			sets = append(sets, "end_minute = ?")
			args = append(args, *body.EndMinute)
		}
		if body.TimeProfileID != nil {
			tpid := strings.TrimSpace(*body.TimeProfileID)
			if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", tpid, "time profile"); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			sets = append(sets, "time_profile_id = ?")
			args = append(args, tpid)
		}
		if len(sets) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no fields"})
			return
		}
		args = append(args, id)
		q := `UPDATE access_time_windows SET ` + strings.Join(sets, ", ") + ` WHERE id = ?`
		res, err := app.DB.Exec(q, args...)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteTimeWindow(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_time_windows WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restListAccessLevels(app *AppContext) gin.HandlerFunc {
	return restListQuery(app, `SELECT id, display_name, time_profile_id, user_group_id, enabled FROM access_levels ORDER BY id`)
}

func restPostAccessLevel(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ID            string `json:"id"`
			DisplayName   string `json:"display_name"`
			TimeProfileID string `json:"time_profile_id"`
			UserGroupID   string `json:"user_group_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if strings.TrimSpace(body.ID) == "" || strings.TrimSpace(body.TimeProfileID) == "" || strings.TrimSpace(body.UserGroupID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id, time_profile_id, user_group_id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", body.TimeProfileID, "time profile"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := techMenuACLEnsureFK(app, "access_user_groups", "id", body.UserGroupID, "user group"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO access_levels (id, display_name, time_profile_id, user_group_id, enabled) VALUES (?, ?, ?, ?, 1)`,
			strings.TrimSpace(body.ID), nullIfEmpty(body.DisplayName), strings.TrimSpace(body.TimeProfileID), strings.TrimSpace(body.UserGroupID))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPatchAccessLevelEnabled(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		v := 0
		if body.Enabled {
			v = 1
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		res, err := app.DB.Exec(`UPDATE access_levels SET enabled = ? WHERE id = ?`, v, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPutAccessLevel(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			DisplayName   string `json:"display_name"`
			TimeProfileID string `json:"time_profile_id"`
			UserGroupID   string `json:"user_group_id"`
			Enabled       *bool  `json:"enabled"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", strings.TrimSpace(body.TimeProfileID), "time profile"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := techMenuACLEnsureFK(app, "access_user_groups", "id", strings.TrimSpace(body.UserGroupID), "user group"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		en := 1
		if body.Enabled != nil {
			if !*body.Enabled {
				en = 0
			}
		} else {
			_ = app.DB.QueryRow(`SELECT enabled FROM access_levels WHERE id = ?`, id).Scan(&en)
		}
		res, err := app.DB.Exec(`UPDATE access_levels SET display_name = ?, time_profile_id = ?, user_group_id = ?, enabled = ? WHERE id = ?`,
			nullIfEmpty(body.DisplayName), strings.TrimSpace(body.TimeProfileID), strings.TrimSpace(body.UserGroupID), en, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteAccessLevel(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_levels WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restListLevelTargets(app *AppContext) gin.HandlerFunc {
	return restListQuery(app, `SELECT id, access_level_id, door_id, door_group_id, elevator_id, elevator_group_id FROM access_level_targets ORDER BY access_level_id, id`)
}

func restPostLevelTarget(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			Kind          string `json:"kind"`
			AccessLevelID string `json:"access_level_id"`
			TargetID      string `json:"target_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		kind := strings.ToLower(strings.TrimSpace(body.Kind))
		lid := strings.TrimSpace(body.AccessLevelID)
		tid := strings.TrimSpace(body.TargetID)
		if lid == "" || tid == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "access_level_id and target_id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_levels", "id", lid, "access level"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var err error
		switch kind {
		case "door":
			if err = techMenuACLEnsureFK(app, "access_doors", "id", tid, "door"); err != nil {
				break
			}
			_, err = app.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, ?, NULL, NULL, NULL)`, lid, tid)
		case "door_group":
			if err = techMenuACLEnsureFK(app, "access_door_groups", "id", tid, "door group"); err != nil {
				break
			}
			_, err = app.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, ?, NULL, NULL)`, lid, tid)
		case "elevator":
			if err = techMenuACLEnsureFK(app, "access_elevators", "id", tid, "elevator"); err != nil {
				break
			}
			_, err = app.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, NULL, ?, NULL)`, lid, tid)
		case "elevator_group":
			if err = techMenuACLEnsureFK(app, "access_elevator_groups", "id", tid, "elevator group"); err != nil {
				break
			}
			_, err = app.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, NULL, NULL, ?)`, lid, tid)
		default:
			err = fmt.Errorf("kind must be door, door_group, elevator, or elevator_group")
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteLevelTarget(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_level_targets WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostExceptionCalendar(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Priority    int    `json:"priority"`
			SourceNote  string `json:"source_note"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id := strings.TrimSpace(body.ID)
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`
			INSERT INTO access_exception_calendars (id, display_name, priority, enabled, source_note)
			VALUES (?, ?, ?, 1, ?)
			ON CONFLICT(id) DO UPDATE SET
				display_name = excluded.display_name,
				priority = excluded.priority,
				source_note = excluded.source_note`,
			id, nullStringIfEmpty(body.DisplayName), body.Priority, nullStringIfEmpty(body.SourceNote))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPatchExceptionCalendar(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			DisplayName *string `json:"display_name"`
			Priority    *int    `json:"priority"`
			Enabled     *bool   `json:"enabled"`
			SourceNote  *string `json:"source_note"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		sets := []string{}
		args := []any{}
		if body.DisplayName != nil {
			sets = append(sets, "display_name = ?")
			args = append(args, nullStringIfEmpty(*body.DisplayName))
		}
		if body.Priority != nil {
			sets = append(sets, "priority = ?")
			args = append(args, *body.Priority)
		}
		if body.Enabled != nil {
			v := 0
			if *body.Enabled {
				v = 1
			}
			sets = append(sets, "enabled = ?")
			args = append(args, v)
		}
		if body.SourceNote != nil {
			sets = append(sets, "source_note = ?")
			args = append(args, nullStringIfEmpty(*body.SourceNote))
		}
		if len(sets) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no fields"})
			return
		}
		args = append(args, id)
		q := `UPDATE access_exception_calendars SET ` + strings.Join(sets, ", ") + ` WHERE id = ?`
		res, err := app.DB.Exec(q, args...)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteExceptionCalendar(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_exception_calendars WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restListExceptionDates(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		cal := strings.TrimSpace(c.Query("calendar_id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		var rows *sql.Rows
		var err error
		if cal != "" {
			rows, err = app.DB.Query(`SELECT id, calendar_id, y, m, d, kind, early_close_minute, label FROM access_exception_dates WHERE calendar_id = ? ORDER BY y, m, d`, cal)
		} else {
			rows, err = app.DB.Query(`SELECT id, calendar_id, y, m, d, kind, early_close_minute, label FROM access_exception_dates ORDER BY y, m, d, calendar_id`)
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		data, err := restScanRows(rows)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, data)
	}
}

func restPostExceptionDate(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			CalendarID       string `json:"calendar_id"`
			Date             string `json:"date"`
			Kind             string `json:"kind"`
			EarlyCloseMinute *int   `json:"early_close_minute"`
			Label            string `json:"label"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		calID := strings.TrimSpace(body.CalendarID)
		ds := strings.TrimSpace(body.Date)
		if calID == "" || ds == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "calendar_id and date (YYYY-MM-DD) required"})
			return
		}
		dt, err := time.ParseInLocation("2006-01-02", ds, time.Local)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		y, mth, d := dt.Date()
		kind := strings.ToLower(strings.TrimSpace(body.Kind))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_exception_calendars", "id", calID, "exception calendar"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		switch kind {
		case "full", "full_closure":
			_, err = app.DB.Exec(`
				INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label)
				VALUES (?, ?, ?, ?, 'full_closure', NULL, ?)
				ON CONFLICT(calendar_id, y, m, d) DO UPDATE SET
					kind = excluded.kind,
					early_close_minute = excluded.early_close_minute,
					label = excluded.label`,
				calID, y, int(mth), d, nullStringIfEmpty(body.Label))
		case "early", "early_closure":
			if body.EarlyCloseMinute == nil || *body.EarlyCloseMinute < 0 || *body.EarlyCloseMinute > 1439 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "early_closure requires early_close_minute 0-1439"})
				return
			}
			_, err = app.DB.Exec(`
				INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label)
				VALUES (?, ?, ?, ?, 'early_closure', ?, ?)
				ON CONFLICT(calendar_id, y, m, d) DO UPDATE SET
					kind = excluded.kind,
					early_close_minute = excluded.early_close_minute,
					label = excluded.label`,
				calID, y, int(mth), d, *body.EarlyCloseMinute, nullStringIfEmpty(body.Label))
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "kind must be full_closure or early_closure"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteExceptionDate(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		res, err := app.DB.Exec(`DELETE FROM access_exception_dates WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostElevatorPinFloor(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			PIN         string `json:"pin"`
			ElevatorID  string `json:"elevator_id"`
			FloorIndex  int    `json:"floor_index"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pin := strings.TrimSpace(body.PIN)
		eid := strings.TrimSpace(body.ElevatorID)
		if pin == "" || eid == "" || body.FloorIndex < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pin, elevator_id, floor_index>=0 required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_pins", "pin", pin, "PIN"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := techMenuACLEnsureFK(app, "access_elevators", "id", eid, "elevator"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO access_elevator_pin_floors (pin, elevator_id, floor_index) VALUES (?, ?, ?)`, pin, eid, body.FloorIndex)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteElevatorPinFloor(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		eid := strings.TrimSpace(c.Query("elevator_id"))
		pin := strings.TrimSpace(c.Query("pin"))
		fi := strings.TrimSpace(c.Query("floor_index"))
		if eid == "" || pin == "" || fi == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "query elevator_id, pin, floor_index required"})
			return
		}
		n, err := strconv.Atoi(fi)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid floor_index"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err = app.DB.Exec(`DELETE FROM access_elevator_pin_floors WHERE elevator_id = ? AND pin = ? AND floor_index = ?`, eid, pin, n)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostElevatorFloorLabel(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ElevatorID string `json:"elevator_id"`
			FloorIndex int    `json:"floor_index"`
			FloorName  string `json:"floor_name"`
			RelayPin   *int   `json:"relay_pin"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		eid := strings.TrimSpace(body.ElevatorID)
		if eid == "" || body.FloorIndex < 0 || strings.TrimSpace(body.FloorName) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "elevator_id, floor_index, floor_name required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_elevators", "id", eid, "elevator"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		var err error
		if body.RelayPin != nil {
			_, err = app.DB.Exec(`INSERT OR REPLACE INTO access_elevator_floor_labels (elevator_id, floor_index, floor_name, relay_pin) VALUES (?, ?, ?, ?)`, eid, body.FloorIndex, strings.TrimSpace(body.FloorName), *body.RelayPin)
		} else {
			_, err = app.DB.Exec(`INSERT OR REPLACE INTO access_elevator_floor_labels (elevator_id, floor_index, floor_name, relay_pin) VALUES (?, ?, ?, NULL)`, eid, body.FloorIndex, strings.TrimSpace(body.FloorName))
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPutElevatorFloorLabel(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		eid := strings.TrimSpace(c.Param("elevator_id"))
		fi, err := strconv.Atoi(c.Param("floor_index"))
		if err != nil || fi < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid floor_index"})
			return
		}
		var body struct {
			FloorName string `json:"floor_name"`
			RelayPin  *int   `json:"relay_pin"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		var exerr error
		if body.RelayPin != nil {
			_, exerr = app.DB.Exec(`UPDATE access_elevator_floor_labels SET floor_name = ?, relay_pin = ? WHERE elevator_id = ? AND floor_index = ?`, strings.TrimSpace(body.FloorName), *body.RelayPin, eid, fi)
		} else {
			_, exerr = app.DB.Exec(`UPDATE access_elevator_floor_labels SET floor_name = ? WHERE elevator_id = ? AND floor_index = ?`, strings.TrimSpace(body.FloorName), eid, fi)
		}
		if exerr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": exerr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteElevatorFloorLabel(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		eid := strings.TrimSpace(c.Param("elevator_id"))
		fi, err := strconv.Atoi(c.Param("floor_index"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid floor_index"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err = app.DB.Exec(`DELETE FROM access_elevator_floor_labels WHERE elevator_id = ? AND floor_index = ?`, eid, fi)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostElevatorFloorGroup(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ID          string `json:"id"`
			ElevatorID  string `json:"elevator_id"`
			DisplayName string `json:"display_name"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id := strings.TrimSpace(body.ID)
		eid := strings.TrimSpace(body.ElevatorID)
		if id == "" || eid == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id and elevator_id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_elevators", "id", eid, "elevator"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO access_elevator_floor_groups (id, elevator_id, display_name) VALUES (?, ?, ?)`, id, eid, nullIfEmpty(body.DisplayName))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPatchElevatorFloorGroup(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			DisplayName *string `json:"display_name"`
			ElevatorID  *string `json:"elevator_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		sets := []string{}
		args := []any{}
		if body.DisplayName != nil {
			sets = append(sets, "display_name = ?")
			args = append(args, nullIfEmpty(*body.DisplayName))
		}
		if body.ElevatorID != nil {
			eid := strings.TrimSpace(*body.ElevatorID)
			if err := techMenuACLEnsureFK(app, "access_elevators", "id", eid, "elevator"); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			sets = append(sets, "elevator_id = ?")
			args = append(args, eid)
		}
		if len(sets) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no fields"})
			return
		}
		args = append(args, id)
		res, err := app.DB.Exec(`UPDATE access_elevator_floor_groups SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteElevatorFloorGroup(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_elevator_floor_groups WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostElevatorFloorGroupMember(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			GroupID    string `json:"group_id"`
			FloorIndex int    `json:"floor_index"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		gid := strings.TrimSpace(body.GroupID)
		if gid == "" || body.FloorIndex < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "group_id and floor_index required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_elevator_floor_groups", "id", gid, "floor group"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO access_elevator_floor_group_members (group_id, floor_index) VALUES (?, ?)`, gid, body.FloorIndex)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteElevatorFloorGroupMember(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		gid := strings.TrimSpace(c.Param("group_id"))
		fi, err := strconv.Atoi(c.Param("floor_index"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid floor_index"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err = app.DB.Exec(`DELETE FROM access_elevator_floor_group_members WHERE group_id = ? AND floor_index = ?`, gid, fi)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostElevatorPinFloorGroup(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			PIN     string `json:"pin"`
			GroupID string `json:"group_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		pin := strings.TrimSpace(body.PIN)
		gid := strings.TrimSpace(body.GroupID)
		if pin == "" || gid == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "pin and group_id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_pins", "pin", pin, "PIN"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := techMenuACLEnsureFK(app, "access_elevator_floor_groups", "id", gid, "floor group"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO access_elevator_pin_floor_groups (pin, group_id) VALUES (?, ?)`, pin, gid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteElevatorPinFloorGroup(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		pin := strings.TrimSpace(c.Query("pin"))
		gid := strings.TrimSpace(c.Query("group_id"))
		if pin == "" || gid == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "query pin and group_id required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_elevator_pin_floor_groups WHERE pin = ? AND group_id = ?`, pin, gid)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restPostElevatorFloorTimeRule(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ElevatorID    string `json:"elevator_id"`
			FloorIndex    int    `json:"floor_index"`
			TimeProfileID string `json:"time_profile_id"`
			Action        string `json:"action"`
			Enabled       *bool  `json:"enabled"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		eid := strings.TrimSpace(body.ElevatorID)
		if eid == "" || body.FloorIndex < 0 || strings.TrimSpace(body.TimeProfileID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "elevator_id, floor_index, time_profile_id required"})
			return
		}
		act := strings.ToLower(strings.TrimSpace(body.Action))
		if act != "open" && act != "lock" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "action must be open or lock"})
			return
		}
		en := 1
		if body.Enabled != nil && !*body.Enabled {
			en = 0
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, "access_elevators", "id", eid, "elevator"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", strings.TrimSpace(body.TimeProfileID), "time profile"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		res, err := app.DB.Exec(`INSERT INTO access_elevator_floor_time_rules (elevator_id, floor_index, time_profile_id, action, enabled) VALUES (?, ?, ?, ?, ?)`,
			eid, body.FloorIndex, strings.TrimSpace(body.TimeProfileID), act, en)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, _ := res.LastInsertId()
		c.JSON(http.StatusOK, gin.H{"ok": true, "id": id})
	}
}

func restPatchElevatorFloorTimeRule(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			Action        *string `json:"action"`
			Enabled       *bool   `json:"enabled"`
			TimeProfileID *string `json:"time_profile_id"`
			FloorIndex    *int    `json:"floor_index"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		sets := []string{}
		args := []any{}
		if body.Action != nil {
			act := strings.ToLower(strings.TrimSpace(*body.Action))
			if act != "open" && act != "lock" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid action"})
				return
			}
			sets = append(sets, "action = ?")
			args = append(args, act)
		}
		if body.Enabled != nil {
			v := 0
			if *body.Enabled {
				v = 1
			}
			sets = append(sets, "enabled = ?")
			args = append(args, v)
		}
		if body.TimeProfileID != nil {
			tpid := strings.TrimSpace(*body.TimeProfileID)
			if err := techMenuACLEnsureFK(app, "access_time_profiles", "id", tpid, "time profile"); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			sets = append(sets, "time_profile_id = ?")
			args = append(args, tpid)
		}
		if body.FloorIndex != nil {
			if *body.FloorIndex < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid floor_index"})
				return
			}
			sets = append(sets, "floor_index = ?")
			args = append(args, *body.FloorIndex)
		}
		if len(sets) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no fields"})
			return
		}
		args = append(args, id)
		res, err := app.DB.Exec(`UPDATE access_elevator_floor_time_rules SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

func restDeleteElevatorFloorTimeRule(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM access_elevator_floor_time_rules WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}
