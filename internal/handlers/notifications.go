package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
)

// visibleClause returns a SQL condition (and its args) restricting
// notifications to what the current user may see: global rows (project_id='')
// plus rows for projects in their project_access. super_admin / 'all' access
// sees everything (R12 security: any logged-in user could read others'
// project alerts before). The condition uses $1 for its single arg.
func visibleClause(r *http.Request) (string, []interface{}) {
	uid := r.Header.Get("X-User-ID")
	if r.Header.Get("X-Role") == "super_admin" {
		return "", nil
	}
	// 精确成员判断 — 子串匹配会越权（"alliance" 含 "all"）(R24)
	var hasAll bool
	db.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM jsonb_array_elements_text(
		COALESCE((SELECT project_access FROM users WHERE id=$1), '[]'::jsonb)) WHERE value = 'all')`, uid).Scan(&hasAll)
	if hasAll {
		return "", nil
	}
	return "COALESCE(project_id,'') = '' OR project_id = ANY (SELECT jsonb_array_elements_text(project_access::jsonb) FROM users WHERE id=$1)", []interface{}{uid}
}

func ListNotifications(w http.ResponseWriter, r *http.Request) {
	ntype := r.URL.Query().Get("type")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 { page = 1 }
	if pageSize < 1 || pageSize > 50 { pageSize = 20 }
	offset := (page - 1) * pageSize
	var conds []string
	var args []interface{}
	if vis, visArgs := visibleClause(r); vis != "" {
		conds = append(conds, vis)
		args = append(args, visArgs...)
	}
	if ntype != "" && ntype != "all" {
		conds = append(conds, "type=$"+strconv.Itoa(len(args)+1))
		args = append(args, ntype)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	query := "SELECT id, COALESCE(project_id,''), type, title, content, COALESCE(is_read,false), COALESCE(created_at::text,'') FROM notifications" + where + " ORDER BY created_at DESC LIMIT $" + strconv.Itoa(len(args)+1) + " OFFSET $" + strconv.Itoa(len(args)+2)
	args = append(args, pageSize, offset)
	rows, err := db.DB.Query(query, args...)
	if err != nil { auth.JSONOK(w, map[string]interface{}{"notifications": []models.Notification{}, "unread_count": 0, "total": 0, "page": page, "page_size": pageSize}); return }
	defer rows.Close()
	ns := []models.Notification{}
	for rows.Next() {
		var n models.Notification
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.Type, &n.Title, &n.Content, &n.Read, &n.CreatedAt); err != nil {
			continue
		}
		ns = append(ns, n)
	}
	if ns == nil { ns = []models.Notification{} }
	var unreadCount int
	visCnt, cntArgs := visibleClauseCount(r)
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE is_read=false"+visCnt, cntArgs...).Scan(&unreadCount)
	auth.JSONOK(w, map[string]interface{}{"notifications": ns, "unread_count": unreadCount, "total": len(ns), "page": page, "page_size": pageSize})
}

// visibleClauseCount mirrors visibleClause for the unread-count query and
// returns the uid to bind to the fragment's $1 (the count query had no bound
// args, so unread_count was silently always 0 — QA round 2 HIGH).
func visibleClauseCount(r *http.Request) (string, []interface{}) {
	uid := r.Header.Get("X-User-ID")
	if r.Header.Get("X-Role") == "super_admin" {
		return "", nil
	}
	restrict := " AND (COALESCE(project_id,'') = '' OR project_id = ANY (SELECT jsonb_array_elements_text(project_access::jsonb) FROM users WHERE id=$1))"
	var hasAll bool
	if err := db.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM jsonb_array_elements_text(
		COALESCE((SELECT project_access FROM users WHERE id=$1), '[]'::jsonb)) WHERE value = 'all')`, uid).Scan(&hasAll); err != nil {
		// fail-closed：查询失败不得按全量放行 (R25)
		return restrict, []interface{}{uid}
	}
	if hasAll {
		return "", nil
	}
	return restrict, []interface{}{uid}
}

// canTouchNotification reports whether the user may read/delete the row.
func canTouchNotification(r *http.Request, nid string) bool {
	uid := r.Header.Get("X-User-ID")
	if r.Header.Get("X-Role") == "super_admin" {
		return true
	}
	var pid string
	if err := db.DB.QueryRow("SELECT COALESCE(project_id,'') FROM notifications WHERE id=$1", nid).Scan(&pid); err != nil {
		return false
	}
	if pid == "" {
		return true // global row
	}
	// 精确匹配：JSON 文本子串匹配会越权（"app" 命中 "app2"、"all" 命中 "fall"）(R24)
	var hasAccess bool
	err := db.DB.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM jsonb_array_elements_text(
			COALESCE((SELECT project_access FROM users WHERE id=$1), '[]'::jsonb)
		) AS pid WHERE pid IN ('all', $2)
	)`, uid, pid).Scan(&hasAccess)
	if err != nil {
		return false // fail-closed：查询失败不得放行 (R25)
	}
	return hasAccess
}

func ReadNotification(w http.ResponseWriter, r *http.Request, nid string) {
	if !canTouchNotification(r, nid) {
		auth.JSONErr(w, 403, "无权操作该通知")
		return
	}
	db.DB.Exec("UPDATE notifications SET is_read=true WHERE id=$1", nid)
	auth.JSONOK(w, map[string]string{"read": nid})
}

func ReadAllNotifications(w http.ResponseWriter, r *http.Request) {
	uid := r.Header.Get("X-User-ID")
	if r.Header.Get("X-Role") == "super_admin" {
		db.DB.Exec("UPDATE notifications SET is_read=true")
	} else {
		var hasAll bool
		db.DB.QueryRow(`SELECT EXISTS (SELECT 1 FROM jsonb_array_elements_text(
			COALESCE((SELECT project_access FROM users WHERE id=$1), '[]'::jsonb)) WHERE value = 'all')`, uid).Scan(&hasAll)
		if hasAll {
			db.DB.Exec("UPDATE notifications SET is_read=true")
		} else {
			db.DB.Exec("UPDATE notifications SET is_read=true WHERE COALESCE(project_id,'') = '' OR project_id = ANY (SELECT jsonb_array_elements_text(project_access::jsonb) FROM users WHERE id=$1)", uid)
		}
	}
	auth.JSONOK(w, map[string]string{"status": "ok"})
}

func DeleteNotification(w http.ResponseWriter, r *http.Request, nid string) {
	if !canTouchNotification(r, nid) {
		auth.JSONErr(w, 403, "无权操作该通知")
		return
	}
	db.DB.Exec("DELETE FROM notifications WHERE id=$1", nid)
	auth.JSONOK(w, map[string]string{"deleted": nid})
}
