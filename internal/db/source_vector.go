package db

import (
	"bytes"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VectorSource implements DataSource for Qdrant vector databases.
// DSN format: qdrant://host:6333 (REST API on the same port).
// Collections = collections; rows = points with payload flattened.
type VectorSource struct {
	dsn string
}

func (s *VectorSource) base() string {
	u := strings.TrimSuffix(s.dsn, "/")
	u = strings.Replace(u, "qdrant://", "http://", 1)
	return u
}

func (s *VectorSource) do(method, path string, body interface{}) ([]byte, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.base()+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("qdrant error %d: %s", resp.StatusCode, buf.String())
	}
	return buf.Bytes(), nil
}

func (s *VectorSource) ListCollections() ([]string, error) {
	data, err := s.do("GET", "/collections", nil)
	if err != nil {
		return []string{}, err
	}
	var out struct {
		Result struct {
			Collections []struct {
				Name string `json:"name"`
			} `json:"collections"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return []string{}, err
	}
	names := []string{}
	for _, c := range out.Result.Collections {
		names = append(names, c.Name)
	}
	return names, nil
}

// newUUIDv4 returns a random UUID string (Qdrant point IDs accept UUIDs).
func newUUIDv4() string {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// scrollPoints fetches points from a collection.
func (s *VectorSource) scrollPoints(collection string, limit int) ([]map[string]interface{}, error) {
	body := map[string]interface{}{"limit": limit, "with_payload": true}
	data, err := s.do("POST", fmt.Sprintf("/collections/%s/points/scroll", url.PathEscape(collection)), body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Points []struct {
				ID      interface{}            `json:"id"`
				Payload map[string]interface{} `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	rows := []map[string]interface{}{}
	for _, p := range out.Result.Points {
		row := map[string]interface{}{"id": fmt.Sprintf("%v", p.ID)}
		for k, v := range p.Payload {
			row[k] = v
		}
		rows = append(rows, row)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return rows, nil
}

func (s *VectorSource) ReadItems(collection string, limit, offset int) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	rows, err := s.scrollPoints(collection, 200)
	if err != nil {
		return nil, nil, "", err
	}
	colSet := map[string]bool{"id": true}
	for _, r := range rows {
		for k := range r {
			colSet[k] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		cols = append(cols, c)
	}
	return rows, cols, "id", nil
}

// ensureCollection auto-creates a collection sized for vectorSize dimensions
// with Cosine distance. Used on first insert/update for a fresh collection.
func (s *VectorSource) ensureCollection(collection string, vectorSize int) error {
	create := map[string]interface{}{
		"vectors": map[string]interface{}{"size": vectorSize, "distance": "Cosine"},
	}
	_, err := s.do("PUT", fmt.Sprintf("/collections/%s", url.PathEscape(collection)), create)
	return err
}

func (s *VectorSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	id := data["id"]
	if id == nil {
		id = newUUIDv4()
	}
	payload := map[string]interface{}{}
	var vector []interface{}
	for k, v := range data {
		if k == "vector" {
			if arr, ok := v.([]interface{}); ok {
				vector = arr
			}
			continue
		}
		if k != "id" {
			payload[k] = v
		}
	}
	body := map[string]interface{}{"points": []map[string]interface{}{{"id": id, "payload": payload}}}
	if vector != nil {
		body["points"].([]map[string]interface{})[0]["vector"] = vector
	}
	_, err := s.do("PUT", fmt.Sprintf("/collections/%s/points?wait=true", url.PathEscape(collection)), body)
	if err != nil && vector != nil && strings.Contains(err.Error(), "doesn't exist") {
		// Auto-create the collection from the first vector's size
		if cerr := s.ensureCollection(collection, len(vector)); cerr != nil {
			return cerr
		}
		_, err = s.do("PUT", fmt.Sprintf("/collections/%s/points?wait=true", url.PathEscape(collection)), body)
	}
	return err
}

func (s *VectorSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	payload := map[string]interface{}{}
	for k, v := range data {
		if k != "id" && k != "vector" {
			payload[k] = v
		}
	}
	body := map[string]interface{}{"points": []map[string]interface{}{{"id": pkVal, "payload": payload}}}
	var vector []interface{}
	if v, ok := data["vector"].([]interface{}); ok {
		vector = v
		body["points"].([]map[string]interface{})[0]["vector"] = v
	}
	_, err := s.do("PUT", fmt.Sprintf("/collections/%s/points?wait=true", url.PathEscape(collection)), body)
	if err != nil && vector != nil && strings.Contains(err.Error(), "doesn't exist") {
		if cerr := s.ensureCollection(collection, len(vector)); cerr != nil {
			return cerr
		}
		_, err = s.do("PUT", fmt.Sprintf("/collections/%s/points?wait=true", url.PathEscape(collection)), body)
	}
	return err
}

func (s *VectorSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	body := map[string]interface{}{"points": []string{pkVal}}
	_, err := s.do("POST", fmt.Sprintf("/collections/%s/points/delete?wait=true", url.PathEscape(collection)), body)
	return err
}

// Search performs vector similarity search on a collection.
// Returns hits with id, score and payload.
func (s *VectorSource) Search(collection string, vector []float64, topK int) ([]map[string]interface{}, error) {
	if err := validateTable(collection); err != nil {
		return nil, err
	}
	if topK < 1 || topK > 100 {
		topK = 10
	}
	body := map[string]interface{}{"vector": vector, "limit": topK, "with_payload": true}
	data, err := s.do("POST", fmt.Sprintf("/collections/%s/points/search", url.PathEscape(collection)), body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result []struct {
			ID      interface{}            `json:"id"`
			Score   float64                `json:"score"`
			Payload map[string]interface{} `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	hits := []map[string]interface{}{}
	for _, h := range out.Result {
		row := map[string]interface{}{"id": fmt.Sprintf("%v", h.ID), "score": h.Score}
		for k, v := range h.Payload {
			row[k] = v
		}
		hits = append(hits, row)
	}
	if hits == nil {
		hits = []map[string]interface{}{}
	}
	return hits, nil
}

func (s *VectorSource) CountItems(collection string) (int, error) {
	return 0, fmt.Errorf("vector source has no row count")
}
