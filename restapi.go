package main

import (
	"bytes"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var restHTTPClient = &http.Client{Timeout: 60 * time.Second}

func startWebServer(app *AppContext) *http.Server {
	app.configMu.RLock()
	enabled := app.Config.RestAPIEnabled
	addr := strings.TrimSpace(app.Config.RestAPIListenAddr)
	app.configMu.RUnlock()
	if !enabled {
		log.Println("INFO: HTTP server not started (rest_api_enabled=false).")
		return &http.Server{}
	}
	if addr == "" {
		addr = ":8080"
	}
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/admin", func(c *gin.Context) {
		c.String(http.StatusOK, "Local configuration interface (use /api/v1 with a valid token).")
	})

	v1 := r.Group("/api/v1")
	v1.Use(restAPIMiddlewareRequireToken(app))
	registerRestAPIV1(v1, app)

	srv := &http.Server{Addr: addr, Handler: r}
	go func() {
		log.Printf("INFO: HTTP REST API listening on %s (secured paths under /api/v1).", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("CRITICAL: HTTP server: %v", err)
		}
	}()
	return srv
}

func restAPIMiddlewareRequireToken(app *AppContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		app.configMu.RLock()
		tok := strings.TrimSpace(app.Config.RestAPIToken)
		app.configMu.RUnlock()
		if tok == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "rest_api_token is not configured; set device.rest_api_token in JSON or cfg set rest_api_token …"})
			c.Abort()
			return
		}
		hdr := strings.TrimSpace(c.GetHeader("Authorization"))
		var presented string
		if strings.HasPrefix(strings.ToLower(hdr), "bearer ") {
			presented = strings.TrimSpace(hdr[7:])
		}
		if presented == "" {
			presented = strings.TrimSpace(c.GetHeader("X-Api-Token"))
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(tok)) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
			c.Abort()
			return
		}
		c.Next()
	}
}

func centralConfigURL(app *AppContext) (string, error) {
	app.configMu.RLock()
	base := strings.TrimRight(strings.TrimSpace(app.Config.CentralServerBaseURL), "/")
	path := strings.TrimSpace(app.Config.CentralServerConfigPath)
	app.configMu.RUnlock()
	if base == "" {
		return "", fmt.Errorf("central_server_base_url is empty")
	}
	if path == "" {
		path = "/virtualkeyz2/v1/config"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path, nil
}

func registerRestAPIV1(g *gin.RouterGroup, app *AppContext) {
	g.GET("/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": SoftwareVersion, "release_utc": SoftwareReleaseUTC})
	})

	// --- Device JSON (cfg list / cfg save / cfg reload / cfg set) ---
	g.GET("/config", func(c *gin.Context) {
		doc := buildPersistFile(app)
		c.JSON(http.StatusOK, doc)
	})
	g.PATCH("/config", func(c *gin.Context) {
		var raw virtualkeyz2JSON
		if err := c.ShouldBindJSON(&raw); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		app.configMu.Lock()
		err := applyVirtualKeyz2JSON(app, &raw)
		app.configMu.Unlock()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "hint": "POST /api/v1/config/apply to refresh MQTT/log filter; POST /api/v1/config/save to persist JSON"})
	})
	g.PATCH("/config/keys", func(c *gin.Context) {
		var m map[string]string
		if err := c.ShouldBindJSON(&m); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		for k, v := range m {
			if err := techMenuCfgSetValue(app, strings.ToLower(strings.TrimSpace(k)), v); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("key %q: %v", k, err)})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	g.POST("/config/apply", func(c *gin.Context) {
		applyInMemoryConfigLive(app)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	g.POST("/config/save", func(c *gin.Context) {
		if err := saveVirtualKeyz2Config(app); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "path": effectiveConfigPath(app)})
	})
	g.POST("/config/reload", func(c *gin.Context) {
		if err := reloadVirtualKeyz2ConfigLive(app); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// --- Central server (device JSON same shape as virtualkeyz2.json) ---
	g.GET("/central/config-url", func(c *gin.Context) {
		u, err := centralConfigURL(app)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"url": u})
	})
	g.POST("/central/config/pull", func(c *gin.Context) {
		u, err := centralConfigURL(app)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		app.configMu.RLock()
		bearer := strings.TrimSpace(app.Config.CentralServerBearerToken)
		app.configMu.RUnlock()
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, u, nil)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := restHTTPClient.Do(req)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("central HTTP %d", resp.StatusCode), "body": string(body)})
			return
		}
		var raw virtualkeyz2JSON
		if err := json.Unmarshal(body, &raw); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("parse central JSON: %v", err)})
			return
		}
		app.configMu.Lock()
		err = applyVirtualKeyz2JSON(app, &raw)
		app.configMu.Unlock()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		applyInMemoryConfigLive(app)
		c.JSON(http.StatusOK, gin.H{"ok": true, "applied": true})
	})
	g.POST("/central/config/push", func(c *gin.Context) {
		u, err := centralConfigURL(app)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		app.configMu.RLock()
		bearer := strings.TrimSpace(app.Config.CentralServerBearerToken)
		app.configMu.RUnlock()
		doc := buildPersistFile(app)
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPut, u, bytes.NewReader(b))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := restHTTPClient.Do(req)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("central HTTP %d", resp.StatusCode), "body": string(rb)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// --- ACL overview (acl summary) ---
	g.GET("/acl/summary", func(c *gin.Context) {
		if app.DB == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
			return
		}
		app.configMu.RLock()
		out := gin.H{
			"access_control_door_id":              strings.TrimSpace(app.Config.AccessControlDoorID),
			"access_control_elevator_id":          strings.TrimSpace(app.Config.AccessControlElevatorID),
			"access_schedule_enforce":             app.Config.AccessScheduleEnforce,
			"access_schedule_apply_to_fallback_pin": app.Config.AccessScheduleApplyToFallbackPin,
		}
		app.configMu.RUnlock()
		counts := map[string]string{
			"doors":                    "access_doors",
			"door_groups":              "access_door_groups",
			"door_group_members":       "access_door_group_members",
			"elevators":                "access_elevators",
			"elevator_groups":          "access_elevator_groups",
			"elevator_group_members":   "access_elevator_group_members",
			"pins":                     "access_pins",
			"user_groups":              "access_user_groups",
			"user_group_members":       "access_user_group_members",
			"time_profiles":            "access_time_profiles",
			"time_windows":             "access_time_windows",
			"access_levels":            "access_levels",
			"level_targets":            "access_level_targets",
			"exception_calendars":      "access_exception_calendars",
			"exception_dates":          "access_exception_dates",
			"elevator_pin_floors":      "access_elevator_pin_floors",
			"elevator_floor_labels":    "access_elevator_floor_labels",
			"elevator_floor_groups":    "access_elevator_floor_groups",
			"elevator_floor_group_members": "access_elevator_floor_group_members",
			"elevator_pin_floor_groups":    "access_elevator_pin_floor_groups",
			"elevator_floor_time_rules":    "access_elevator_floor_time_rules",
			"audit_logs":               "logs",
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		n := make(map[string]int)
		for label, table := range counts {
			var ct int
			_ = app.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&ct)
			n[label] = ct
		}
		out["table_counts"] = n
		c.JSON(http.StatusOK, out)
	})

	g.POST("/acl/bind", func(c *gin.Context) {
		var body struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		parts := []string{"acl", "bind", body.Kind, body.ID}
		if err := techMenuACLCmdBind(app, parts); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// --- Simple id + display_name tables ---
	restSimpleEntity(g, app, "doors", "access_doors")
	restSimpleEntity(g, app, "door-groups", "access_door_groups")
	restSimpleEntity(g, app, "elevators", "access_elevators")
	restSimpleEntity(g, app, "elevator-groups", "access_elevator_groups")
	restSimpleEntity(g, app, "user-groups", "access_user_groups")

	// --- Group membership composite ---
	restComposite2(g, app, "door-group-members", "access_door_group_members", "door_group_id", "door_id",
		"access_door_groups", "access_doors", "id", "door_group_id", "door_id")
	restComposite2(g, app, "elevator-group-members", "access_elevator_group_members", "elevator_group_id", "elevator_id",
		"access_elevator_groups", "access_elevators", "id", "elevator_group_id", "elevator_id")
	restComposite2(g, app, "user-group-members", "access_user_group_members", "group_id", "pin",
		"access_user_groups", "access_pins", "pin", "group_id", "pin")

	// --- PINs (pin path segment URL-decoded; avoid slashes in PINs or use query on list/detail) ---
	g.GET("/acl/pins", restListPins(app))
	g.GET("/acl/pins/*pinpath", restGetPin(app))
	g.POST("/acl/pins", restPostPin(app))
	g.POST("/acl/pins/temporary", restPostPinTemporary(app))
	g.PATCH("/acl/pins/*pinpath", restPatchPin(app))
	g.DELETE("/acl/pins/*pinpath", restDeletePin(app))

	// --- Time profiles & windows ---
	g.GET("/acl/time-profiles", restQueryTable(app, "access_time_profiles", "id", "display_name", "iana_timezone", "respects_exception_calendar"))
	g.POST("/acl/time-profiles", restPostTimeProfile(app))
	g.PATCH("/acl/time-profiles/:id/respects-exceptions", restPatchProfileRespectsExceptions(app))
	g.DELETE("/acl/time-profiles/:id", restDeleteTimeProfile(app))

	g.GET("/acl/time-windows", restListTimeWindows(app))
	g.POST("/acl/time-windows", restPostTimeWindow(app))
	g.PATCH("/acl/time-windows/:id", restPatchTimeWindow(app))
	g.DELETE("/acl/time-windows/:id", restDeleteTimeWindow(app))

	// --- Access levels & targets ---
	g.GET("/acl/access-levels", restListAccessLevels(app))
	g.POST("/acl/access-levels", restPostAccessLevel(app))
	g.PATCH("/acl/access-levels/:id/enabled", restPatchAccessLevelEnabled(app))
	g.PUT("/acl/access-levels/:id", restPutAccessLevel(app))
	g.DELETE("/acl/access-levels/:id", restDeleteAccessLevel(app))

	g.GET("/acl/level-targets", restListLevelTargets(app))
	g.POST("/acl/level-targets", restPostLevelTarget(app))
	g.DELETE("/acl/level-targets/:id", restDeleteLevelTarget(app))

	// --- Exception calendars ---
	g.GET("/acl/exception-calendars", restQueryTable(app, "access_exception_calendars", "id", "display_name", "priority", "enabled", "source_note"))
	g.POST("/acl/exception-calendars", restPostExceptionCalendar(app))
	g.PATCH("/acl/exception-calendars/:id", restPatchExceptionCalendar(app))
	g.DELETE("/acl/exception-calendars/:id", restDeleteExceptionCalendar(app))

	g.GET("/acl/exception-dates", restListExceptionDates(app))
	g.POST("/acl/exception-dates", restPostExceptionDate(app))
	g.DELETE("/acl/exception-dates/:id", restDeleteExceptionDate(app))

	// --- Elevator floor ACL ---
	g.GET("/acl/elevator-pin-floors", restListQuery(app, `SELECT pin, elevator_id, floor_index FROM access_elevator_pin_floors ORDER BY elevator_id, pin, floor_index`))
	g.POST("/acl/elevator-pin-floors", restPostElevatorPinFloor(app))
	g.DELETE("/acl/elevator-pin-floors", restDeleteElevatorPinFloor(app))

	g.GET("/acl/elevator-floor-labels", restListQuery(app, `SELECT elevator_id, floor_index, floor_name, relay_pin FROM access_elevator_floor_labels ORDER BY elevator_id, floor_index`))
	g.POST("/acl/elevator-floor-labels", restPostElevatorFloorLabel(app))
	g.PUT("/acl/elevator-floor-labels/:elevator_id/:floor_index", restPutElevatorFloorLabel(app))
	g.DELETE("/acl/elevator-floor-labels/:elevator_id/:floor_index", restDeleteElevatorFloorLabel(app))

	g.GET("/acl/elevator-floor-groups", restListQuery(app, `SELECT id, elevator_id, display_name FROM access_elevator_floor_groups ORDER BY id`))
	g.POST("/acl/elevator-floor-groups", restPostElevatorFloorGroup(app))
	g.PATCH("/acl/elevator-floor-groups/:id", restPatchElevatorFloorGroup(app))
	g.DELETE("/acl/elevator-floor-groups/:id", restDeleteElevatorFloorGroup(app))

	g.GET("/acl/elevator-floor-group-members", restListQuery(app, `SELECT group_id, floor_index FROM access_elevator_floor_group_members ORDER BY group_id, floor_index`))
	g.POST("/acl/elevator-floor-group-members", restPostElevatorFloorGroupMember(app))
	g.DELETE("/acl/elevator-floor-group-members/:group_id/:floor_index", restDeleteElevatorFloorGroupMember(app))

	g.GET("/acl/elevator-pin-floor-groups", restListQuery(app, `SELECT pin, group_id FROM access_elevator_pin_floor_groups ORDER BY pin, group_id`))
	g.POST("/acl/elevator-pin-floor-groups", restPostElevatorPinFloorGroup(app))
	g.DELETE("/acl/elevator-pin-floor-groups", restDeleteElevatorPinFloorGroup(app))

	g.GET("/acl/elevator-floor-time-rules", restListQuery(app, `SELECT id, elevator_id, floor_index, time_profile_id, action, enabled FROM access_elevator_floor_time_rules ORDER BY id`))
	g.POST("/acl/elevator-floor-time-rules", restPostElevatorFloorTimeRule(app))
	g.PATCH("/acl/elevator-floor-time-rules/:id", restPatchElevatorFloorTimeRule(app))
	g.DELETE("/acl/elevator-floor-time-rules/:id", restDeleteElevatorFloorTimeRule(app))
}

func restMustDB(c *gin.Context, app *AppContext) bool {
	if app.DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return false
	}
	return true
}

func restSimpleEntity(g *gin.RouterGroup, app *AppContext, routeName, table string) {
	path := "/acl/" + routeName
	g.GET(path, restQueryTable(app, table, "id", "display_name"))
	g.POST(path, func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
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
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO `+table+` (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(body.DisplayName))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	g.PUT(path+"/:id", func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		var body struct {
			DisplayName string `json:"display_name"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		res, err := app.DB.Exec(`UPDATE `+table+` SET display_name = ? WHERE id = ?`, nullIfEmpty(body.DisplayName), id)
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
	})
	g.DELETE(path+"/:id", func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		id := strings.TrimSpace(c.Param("id"))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM `+table+` WHERE id = ?`, id)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}

// restComposite2 registers list (optional filters), create, and delete for a two-column membership table.
// fkBCol is the column name on fkTableB used for the foreign key (usually "id"; use "pin" for access_pins).
// delP1 and delP2 are Gin path parameter names for DELETE (must match colA/colB values).
func restComposite2(g *gin.RouterGroup, app *AppContext, route, table, colA, colB, fkTableA, fkTableB, fkBCol, delP1, delP2 string) {
	base := "/acl/" + route
	g.GET(base, func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		qa := strings.TrimSpace(c.Query(colA))
		qb := strings.TrimSpace(c.Query(colB))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		var rows *sql.Rows
		var err error
		switch {
		case qa != "" && qb != "":
			rows, err = app.DB.Query(`SELECT `+colA+`, `+colB+` FROM `+table+` WHERE `+colA+` = ? AND `+colB+` = ? ORDER BY 1,2`, qa, qb)
		case qa != "":
			rows, err = app.DB.Query(`SELECT `+colA+`, `+colB+` FROM `+table+` WHERE `+colA+` = ? ORDER BY 2`, qa)
		case qb != "":
			rows, err = app.DB.Query(`SELECT `+colA+`, `+colB+` FROM `+table+` WHERE `+colB+` = ? ORDER BY 1`, qb)
		default:
			rows, err = app.DB.Query(`SELECT `+colA+`, `+colB+` FROM `+table+` ORDER BY 1,2`)
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var out []map[string]string
		for rows.Next() {
			var a, b string
			if err := rows.Scan(&a, &b); err != nil {
				break
			}
			out = append(out, map[string]string{colA: a, colB: b})
		}
		c.JSON(http.StatusOK, out)
	})
	g.POST(base, func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		var body map[string]string
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		a := strings.TrimSpace(body[colA])
		b := strings.TrimSpace(body[colB])
		if a == "" || b == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": colA + " and " + colB + " required"})
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(app, fkTableA, "id", a, "parent"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := techMenuACLEnsureFK(app, fkTableB, fkBCol, b, "member"); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		_, err := app.DB.Exec(`INSERT OR REPLACE INTO `+table+` (`+colA+`, `+colB+`) VALUES (?, ?)`, a, b)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	g.DELETE(base+"/:"+delP1+"/:"+delP2, func(c *gin.Context) {
		if !restMustDB(c, app) {
			return
		}
		a := strings.TrimSpace(c.Param(delP1))
		b := strings.TrimSpace(c.Param(delP2))
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := app.DB.Exec(`DELETE FROM `+table+` WHERE `+colA+` = ? AND `+colB+` = ?`, a, b)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}
