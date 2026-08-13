package handlers

import (
	"net/http"
	"strconv"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
)

func ListNotifications(w http.ResponseWriter, r *http.Request) {
	ntype := r.URL.Query().Get("type")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 { page = 1 }
	if pageSize < 1 || pageSize > 50 { pageSize = 20 }
	offset := (page - 1) * pageSize
	where := ""; var args []interface{}
	if ntype != "" && ntype != "all" { where = " WHERE type=$1"; args = append(args, ntype) }
	query := "SELECT id, COALESCE(project_id,''), type, title, content, COALESCE(is_read,false), COALESCE(created_at::text,'') FROM notifications" + where + " ORDER BY created_at DESC LIMIT $" + strconv.Itoa(len(args)+1) + " OFFSET $" + strconv.Itoa(len(args)+2)
	args = append(args, pageSize, offset)
	rows, err := db.DB.Query(query, args...)
	if err != nil { auth.JSONOK(w, map[string]interface{}{"notifications": []models.Notification{}, "unread_count": 0, "total": 0, "page": page, "page_size": pageSize}); return }
	defer rows.Close()
	ns := []models.Notification{}
	for rows.Next() { var n models.Notification; rows.Scan(&n.ID, &n.ProjectID, &n.Type, &n.Title, &n.Content, &n.Read, &n.CreatedAt); ns = append(ns, n) }
	if ns == nil { ns = []models.Notification{} }
	var unreadCount int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE is_read=false").Scan(&unreadCount)
	auth.JSONOK(w, map[string]interface{}{"notifications": ns, "unread_count": unreadCount, "total": len(ns), "page": page, "page_size": pageSize})
}

func ReadNotification(w http.ResponseWriter, r *http.Request, nid string) {
	db.DB.Exec("UPDATE notifications SET is_read=true WHERE id=$1", nid)
	auth.JSONOK(w, map[string]string{"read": nid})
}

func ReadAllNotifications(w http.ResponseWriter, r *http.Request) {
	db.DB.Exec("UPDATE notifications SET is_read=true")
	auth.JSONOK(w, map[string]string{"status": "ok"})
}

func DeleteNotification(w http.ResponseWriter, r *http.Request, nid string) {
	db.DB.Exec("DELETE FROM notifications WHERE id=$1", nid)
	auth.JSONOK(w, map[string]string{"deleted": nid})
}
