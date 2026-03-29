package main

import (
	"net/http"

	"github.com/graphql-go/graphql"
	gqlhandler "github.com/graphql-go/handler"
)

var gqlChatMsg = graphql.NewObject(graphql.ObjectConfig{
	Name: "ChatMessage",
	Fields: graphql.Fields{
		"id": {Type: graphql.String}, "username": {Type: graphql.String},
		"role": {Type: graphql.String}, "message": {Type: graphql.String}, "sentAt": {Type: graphql.String},
	},
})

var gqlStream = graphql.NewObject(graphql.ObjectConfig{
	Name: "Stream",
	Fields: graphql.Fields{
		"id": {Type: graphql.String}, "roomName": {Type: graphql.String},
		"title": {Type: graphql.String}, "category": {Type: graphql.String},
		"streamer": {Type: graphql.String}, "startedAt": {Type: graphql.String},
		"endedAt": {Type: graphql.String}, "peakViewers": {Type: graphql.Int},
		"durationSec": {Type: graphql.Int}, "live": {Type: graphql.Boolean},
		"viewers": &graphql.Field{
			Type: graphql.Int,
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				src, _ := p.Source.(map[string]interface{})
				rn, _ := src["roomName"].(string)
				mu.RLock()
				rm, ok := rooms[rn]
				mu.RUnlock()
				if !ok {
					return 0, nil
				}
				n := 0
				for _, c := range rm.Clients {
					if c.role == "viewer" {
						n++
					}
				}
				return n, nil
			},
		},
		"chatMessages": &graphql.Field{
			Type: graphql.NewList(gqlChatMsg),
			Args: graphql.FieldConfigArgument{"limit": {Type: graphql.Int, DefaultValue: 100}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if db == nil {
					return []interface{}{}, nil
				}
				src, _ := p.Source.(map[string]interface{})
				sid, _ := src["id"].(string)
				limit, _ := p.Args["limit"].(int)
				if limit <= 0 {
					limit = 100
				}
				rows, err := db.Query(`SELECT id,username,role,message,sent_at FROM chat_messages WHERE stream_id=$1 ORDER BY sent_at ASC LIMIT $2`, sid, limit)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var out []interface{}
				for rows.Next() {
					var id, u, r, msg, ts string
					rows.Scan(&id, &u, &r, &msg, &ts)
					out = append(out, map[string]interface{}{"id": id, "username": u, "role": r, "message": msg, "sentAt": ts})
				}
				if out == nil {
					out = []interface{}{}
				}
				return out, nil
			},
		},
	},
})

var gqlUser = graphql.NewObject(graphql.ObjectConfig{
	Name: "User",
	Fields: graphql.Fields{
		"id": {Type: graphql.String}, "username": {Type: graphql.String},
		"bio": {Type: graphql.String}, "avatarUrl": {Type: graphql.String}, "createdAt": {Type: graphql.String},
		"streams": &graphql.Field{
			Type: graphql.NewList(gqlStream),
			Args: graphql.FieldConfigArgument{"limit": {Type: graphql.Int, DefaultValue: 20}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if db == nil {
					return []interface{}{}, nil
				}
				src, _ := p.Source.(map[string]interface{})
				uid, _ := src["id"].(string)
				limit, _ := p.Args["limit"].(int)
				rows, err := db.Query(`SELECT s.id,s.room_name,s.title,s.category,u.username,s.started_at,s.ended_at,s.peak_viewers,s.duration_sec,(s.ended_at IS NULL) FROM streams s LEFT JOIN users u ON u.id=s.user_id WHERE s.user_id=$1 ORDER BY s.started_at DESC LIMIT $2`, uid, limit)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				return scanStreams(rows), nil
			},
		},
	},
})

var gqlRoot = graphql.NewObject(graphql.ObjectConfig{
	Name: "Query",
	Fields: graphql.Fields{
		"liveStreams": &graphql.Field{Type: graphql.NewList(gqlStream), Resolve: func(p graphql.ResolveParams) (interface{}, error) { return liveFromMemory(), nil }},
		"streams": &graphql.Field{
			Type: graphql.NewList(gqlStream),
			Args: graphql.FieldConfigArgument{"live": {Type: graphql.Boolean}, "category": {Type: graphql.String}, "limit": {Type: graphql.Int, DefaultValue: 50}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				la, ls := p.Args["live"].(bool)
				if (ls && la) || db == nil {
					return liveFromMemory(), nil
				}
				limit, _ := p.Args["limit"].(int)
				cat, _ := p.Args["category"].(string)
				q := `SELECT s.id,s.room_name,s.title,s.category,u.username,s.started_at,s.ended_at,s.peak_viewers,s.duration_sec,(s.ended_at IS NULL) FROM streams s LEFT JOIN users u ON u.id=s.user_id`
				args := []interface{}{}
				where := []string{}
				if ls && !la {
					where = append(where, "s.ended_at IS NOT NULL")
				}
				if cat != "" {
					args = append(args, cat)
					where = append(where, "s.category=$"+n2s(len(args)))
				}
				if len(where) > 0 {
					q += " WHERE " + joinW(where)
				}
				args = append(args, limit)
				q += " ORDER BY s.started_at DESC LIMIT $" + n2s(len(args))
				rows, err := db.Query(q, args...)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				return scanStreams(rows), nil
			},
		},
		"stream": &graphql.Field{
			Type: gqlStream,
			Args: graphql.FieldConfigArgument{"id": {Type: graphql.NewNonNull(graphql.String)}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if db == nil {
					return nil, nil
				}
				rows, err := db.Query(`SELECT s.id,s.room_name,s.title,s.category,u.username,s.started_at,s.ended_at,s.peak_viewers,s.duration_sec,(s.ended_at IS NULL) FROM streams s LEFT JOIN users u ON u.id=s.user_id WHERE s.id=$1`, p.Args["id"].(string))
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				list := scanStreams(rows)
				if len(list) == 0 {
					return nil, nil
				}
				return list[0], nil
			},
		},
		"user": &graphql.Field{
			Type: gqlUser,
			Args: graphql.FieldConfigArgument{"username": {Type: graphql.NewNonNull(graphql.String)}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if db == nil {
					return nil, nil
				}
				un := p.Args["username"].(string)
				var id, av, bio, ca string
				if db.QueryRow(`SELECT id,avatar_url,bio,created_at FROM users WHERE username=$1`, un).Scan(&id, &av, &bio, &ca) != nil {
					return nil, nil
				}
				return map[string]interface{}{"id": id, "username": un, "bio": bio, "avatarUrl": av, "createdAt": ca}, nil
			},
		},
		"chatHistory": &graphql.Field{
			Type: graphql.NewList(gqlChatMsg),
			Args: graphql.FieldConfigArgument{"room": {Type: graphql.NewNonNull(graphql.String)}, "limit": {Type: graphql.Int, DefaultValue: 100}},
			Resolve: func(p graphql.ResolveParams) (interface{}, error) {
				if db == nil {
					return []interface{}{}, nil
				}
				room := p.Args["room"].(string)
				limit, _ := p.Args["limit"].(int)
				var sid string
				if db.QueryRow(`SELECT id FROM streams WHERE room_name=$1 ORDER BY started_at DESC LIMIT 1`, room).Scan(&sid) != nil {
					return []interface{}{}, nil
				}
				rows, err := db.Query(`SELECT id,username,role,message,sent_at FROM chat_messages WHERE stream_id=$1 ORDER BY sent_at ASC LIMIT $2`, sid, limit)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				var out []interface{}
				for rows.Next() {
					var id, u, r, msg, ts string
					rows.Scan(&id, &u, &r, &msg, &ts)
					out = append(out, map[string]interface{}{"id": id, "username": u, "role": r, "message": msg, "sentAt": ts})
				}
				if out == nil {
					out = []interface{}{}
				}
				return out, nil
			},
		},
	},
})

func registerGraphQL(mux *http.ServeMux) {
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: gqlRoot})
	if err != nil {
		panic("GraphQL: " + err.Error())
	}
	h := gqlhandler.New(&gqlhandler.Config{Schema: &schema, Pretty: true, GraphiQL: true})
	mux.Handle("/graphql", gqlCORS(h))
}

func gqlCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func liveFromMemory() []interface{} {
	mu.RLock()
	defer mu.RUnlock()
	var out []interface{}
	for _, rm := range rooms {
		sn, vc := "", 0
		for _, c := range rm.Clients {
			if c.role == "streamer" {
				sn = c.name
			}
			if c.role == "viewer" {
				vc++
			}
		}
		if sn == "" {
			continue
		}
		out = append(out, map[string]interface{}{"id": n2s64(rm.StreamID), "roomName": rm.Name, "title": rm.Title, "category": rm.Category, "streamer": sn, "viewers": vc, "peakViewers": rm.PeakViewers, "startedAt": rm.StartedAt.Format("2006-01-02T15:04:05Z"), "endedAt": nil, "live": true, "durationSec": 0})
	}
	if out == nil {
		out = []interface{}{}
	}
	return out
}

func scanStreams(rows interface {
	Next() bool
	Scan(...interface{}) error
}) []interface{} {
	var out []interface{}
	for rows.Next() {
		var id, rn, title, cat string
		var streamer *string
		var sa string
		var ea *string
		var pv, dur int
		var live bool
		if rows.Scan(&id, &rn, &title, &cat, &streamer, &sa, &ea, &pv, &dur, &live) != nil {
			continue
		}
		sv := ""
		if streamer != nil {
			sv = *streamer
		}
		var eav interface{}
		if ea != nil {
			eav = *ea
		}
		out = append(out, map[string]interface{}{"id": id, "roomName": rn, "title": title, "category": cat, "streamer": sv, "startedAt": sa, "endedAt": eav, "peakViewers": pv, "durationSec": dur, "live": live})
	}
	if out == nil {
		out = []interface{}{}
	}
	return out
}

func n2s(n int) string { return n2s64(int64(n)) }
func n2s64(n int64) string {
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 20)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
func joinW(p []string) string {
	r := ""
	for i, s := range p {
		if i > 0 {
			r += " AND "
		}
		r += s
	}
	return r
}
