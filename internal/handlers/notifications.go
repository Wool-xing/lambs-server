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
	var pa string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", uid).Scan(&pa)
	if strings.Contains(pa, "all") {
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
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE is_read=false"+visibleClauseCount(r)).Scan(&unreadCount)
	auth.JSONOK(w, map[string]interface{}{"notifications": ns, "unread_count": unreadCount, "total": len(ns), "page": page, "page_size": pageSize})
}

// visibleClauseCount mirrors visibleClause for the unread-count query (a
// subquery filter, so no args needed).
func visibleClauseCount(r *http.Request) string {
	uid := r.Header.Get("X-User-ID")
	if r.Header.Get("X-Role") == "super_admin" {
		return ""
	}
	var pa string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", uid).Scan(&pa)
	if strings.Contains(pa, "all") {
		return ""
	}
	return " AND (COALESCE(project_id,'') = '' OR project_id = ANY (SELECT jsonb_array_elements_text(project_access::jsonb) FROM users WHERE id=$1))"
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
	var pa string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", uid).Scan(&pa)
	return strings.Contains(pa, "all") || strings.Contains(pa, pid)
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
		var pa string
		db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", uid).Scan(&pa)
		if strings.Contains(pa, "all") {
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
