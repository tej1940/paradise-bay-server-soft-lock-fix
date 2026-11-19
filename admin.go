package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const maxBackupBytes = 32 << 20

var errDeviceIDAlreadyLinked = errors.New("cet identifiant appareil est déjà rattaché à un autre joueur")

type playerSummary struct {
	UUID                 string  `json:"uuid"`
	Alias                string  `json:"alias"`
	Z2DID                string  `json:"z2did"`
	Nanopods             int64   `json:"nanopods"`
	Gems                 int64   `json:"gems"`
	LifetimeValueDollars float64 `json:"lifetimeValueDollars"`
	LeaderboardTier      string  `json:"leaderboardTier"`
	FragmentCount        int     `json:"fragmentCount"`
	LastSavedAt          string  `json:"lastSavedAt"`
}

type fragmentSummary struct {
	Key       string `json:"key"`
	Hash      string `json:"hash"`
	UpdatedAt string `json:"updatedAt"`
}

type playerBackup struct {
	Version    int                `json:"version"`
	ExportedAt time.Time          `json:"exportedAt"`
	Player     Player             `json:"player"`
	Fragments  []DocumentFragment `json:"fragments"`
}

func registerAdminRoutes(mux *http.ServeMux, store *Store) {
	mux.HandleFunc("GET /admin", serveAdminPanel)
	mux.HandleFunc("GET /admin/", serveAdminPanel)
	mux.HandleFunc("GET /admin/api/players", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		players, err := store.ListPlayers()
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "Impossible de charger les joueurs.")
			return
		}
		summaries := make([]playerSummary, 0, len(players))
		for _, player := range players {
			fragments, err := store.GetPlayerFragments(player.UUID)
			if err != nil {
				writeAdminError(w, http.StatusInternalServerError, "Impossible de charger les sauvegardes.")
				return
			}
			summaries = append(summaries, summarizePlayer(player, fragments))
		}
		writeJSON(w, http.StatusOK, summaries)
	})
	mux.HandleFunc("GET /admin/api/players/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		player, fragments, ok := loadAdminPlayer(w, store, r.PathValue("uuid"))
		if !ok {
			return
		}
		items := make([]fragmentSummary, 0, len(fragments))
		for _, fragment := range fragments {
			items = append(items, fragmentSummary{
				Key:       fragment.Key,
				Hash:      fragment.Hash,
				UpdatedAt: fragment.UpdatedAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"player":    summarizePlayer(player, fragments),
			"fragments": items,
		})
	})
	mux.HandleFunc("PUT /admin/api/players/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		var input struct {
			Alias string `json:"alias"`
			Z2DID string `json:"z2did"`
		}
		if err := decodeAdminJSON(w, r, &input); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		input.Alias = strings.TrimSpace(input.Alias)
		input.Z2DID = strings.TrimSpace(input.Z2DID)
		if input.Alias == "" || len(input.Alias) > 100 || len(input.Z2DID) > 256 {
			writeAdminError(w, http.StatusBadRequest, "Le pseudo doit contenir 1 à 100 caractères et l'identifiant appareil au maximum 256 caractères.")
			return
		}
		err := store.SetPlayerIdentity(r.PathValue("uuid"), input.Alias, input.Z2DID)
		switch {
		case errors.Is(err, errDeviceIDAlreadyLinked):
			writeAdminError(w, http.StatusConflict, err.Error())
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
		case err != nil:
			writeAdminError(w, http.StatusInternalServerError, "Impossible de modifier le joueur.")
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("DELETE /admin/api/players/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		if err := store.DeletePlayer(r.PathValue("uuid")); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "Impossible de supprimer le joueur.")
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /admin/api/players/{uuid}/skip-seal-house-tutorial", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		err := store.SkipDiveTutorial(r.PathValue("uuid"))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
		case err != nil:
			writeAdminError(w, http.StatusInternalServerError, "Impossible de modifier la progression des missions.")
		default:
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		}
	})
	mux.HandleFunc("GET /admin/api/players/{uuid}/backup", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		player, fragments, ok := loadAdminPlayer(w, store, r.PathValue("uuid"))
		if !ok {
			return
		}
		backup := playerBackup{
			Version:    1,
			ExportedAt: time.Now().UTC(),
			Player:     player,
			Fragments:  fragments,
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="paradise-bay-%s.json"`, player.UUID))
		writeJSON(w, http.StatusOK, backup)
	})
	mux.HandleFunc("POST /admin/api/players/{uuid}/restore", func(w http.ResponseWriter, r *http.Request) {
		if !requireLocalAdmin(w, r) {
			return
		}
		uuid := r.PathValue("uuid")
		currentPlayer, err := store.GetPlayerByCID(uuid)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "Impossible de charger le joueur.")
			}
			return
		}
		var backup playerBackup
		if err := decodeAdminJSON(w, r, &backup); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		if backup.Version != 1 || backup.Player.UUID != uuid || strings.TrimSpace(backup.Player.Alias) == "" || backup.Fragments == nil {
			writeAdminError(w, http.StatusBadRequest, "Cette sauvegarde n'est pas compatible avec ce joueur.")
			return
		}
		// A restore must not undo a device relink performed after the backup was made.
		backup.Player.Z2DID = currentPlayer.Z2DID
		backup.Player.CurrentTcID = currentPlayer.CurrentTcID
		seen := make(map[string]struct{}, len(backup.Fragments))
		now := time.Now().UTC().Format("2006-01-02 15:04:05")
		for i := range backup.Fragments {
			fragment := &backup.Fragments[i]
			if fragment.Key == "" || len(fragment.Key) > 2048 || !strings.Contains(fragment.Key, uuid) || !json.Valid([]byte(fragment.BlobJSON)) {
				writeAdminError(w, http.StatusBadRequest, "La sauvegarde contient un fragment invalide ou appartenant à un autre joueur.")
				return
			}
			if _, duplicate := seen[fragment.Key]; duplicate {
				writeAdminError(w, http.StatusBadRequest, "La sauvegarde contient des fragments en double.")
				return
			}
			seen[fragment.Key] = struct{}{}
			hash := sha256.Sum256([]byte(fragment.BlobJSON))
			fragment.Hash = hex.EncodeToString(hash[:])
			fragment.UpdatedAt = now
		}
		if err := store.ReplacePlayerSave(backup.Player, backup.Fragments); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
			} else {
				writeAdminError(w, http.StatusInternalServerError, "La restauration a échoué ; aucune modification n'a été appliquée.")
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

func summarizePlayer(player Player, fragments []DocumentFragment) playerSummary {
	summary := playerSummary{
		UUID:                 player.UUID,
		Alias:                player.Alias,
		Z2DID:                player.Z2DID,
		Nanopods:             player.Nanopods,
		Gems:                 player.Gems,
		LifetimeValueDollars: player.LifetimeValueDollars,
		LeaderboardTier:      player.LeaderboardTier,
		FragmentCount:        len(fragments),
	}
	for _, fragment := range fragments {
		if fragment.UpdatedAt > summary.LastSavedAt {
			summary.LastSavedAt = fragment.UpdatedAt
		}
	}
	return summary
}

func loadAdminPlayer(w http.ResponseWriter, store *Store, uuid string) (Player, []DocumentFragment, bool) {
	player, err := store.GetPlayerByCID(uuid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeAdminError(w, http.StatusNotFound, "Joueur introuvable.")
		} else {
			writeAdminError(w, http.StatusInternalServerError, "Impossible de charger le joueur.")
		}
		return Player{}, nil, false
	}
	fragments, err := store.GetPlayerFragments(uuid)
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "Impossible de charger la sauvegarde.")
		return Player{}, nil, false
	}
	return player, fragments, true
}

func requireLocalAdmin(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		http.Error(w, "Admin panel is available only from this computer.", http.StatusForbidden)
		return false
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, destination interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBackupBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("JSON invalide : %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("le corps doit contenir un seul objet JSON")
	}
	return nil
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		fmt.Printf("failed to write admin response: %v\n", err)
	}
}

func serveAdminPanel(w http.ResponseWriter, r *http.Request) {
	if !requireLocalAdmin(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_, _ = io.WriteString(w, adminPanelHTML)
}

const adminPanelHTML = `<!doctype html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Paradise Bay — Administration</title>
<style>
:root{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;color:#193446;background:#eef5f7;font-synthesis:none}
*{box-sizing:border-box}body{margin:0}header{background:#145b73;color:white;padding:22px max(24px,calc((100% - 1120px)/2))}
header h1{margin:0;font-size:1.55rem}header p{margin:5px 0 0;color:#d4edf3}
main{max-width:1120px;margin:26px auto;padding:0 20px;display:grid;grid-template-columns:minmax(0,1.3fr) minmax(300px,.8fr);gap:20px}
section{background:white;border:1px solid #d5e2e7;border-radius:12px;padding:20px;box-shadow:0 3px 12px #1745590d}
h2{font-size:1.12rem;margin:0 0 14px}button,.button{border:0;border-radius:7px;background:#176b83;color:white;padding:9px 13px;font:inherit;cursor:pointer;text-decoration:none;display:inline-block}
button.secondary,.button.secondary{background:#e5eff2;color:#234957}button.danger{background:#a13b34}
button:disabled{opacity:.55;cursor:wait}.toolbar{display:flex;justify-content:space-between;align-items:center;margin-bottom:12px}
table{width:100%;border-collapse:collapse;font-size:.92rem}th,td{text-align:left;padding:10px 8px;border-bottom:1px solid #e6eef1}th{color:#58717c;font-weight:600}
tr.player{cursor:pointer}tr.player:hover,tr.player.selected{background:#edf7f8}.muted{color:#607985;font-size:.88rem}.empty{padding:18px;text-align:center;color:#607985}
label{display:block;font-weight:600;font-size:.9rem;margin:13px 0 5px}input{width:100%;border:1px solid #bcced5;border-radius:7px;padding:10px;font:inherit}
.actions{display:flex;gap:8px;flex-wrap:wrap;margin-top:16px}.notice{margin:12px 0;padding:10px;border-radius:7px;background:#edf7f8}.notice.error{background:#fbeceb;color:#8d2b25}
.fragments{max-height:250px;overflow:auto;margin-top:8px}.fragments li{font-size:.82rem;overflow-wrap:anywhere;margin:7px 0}
@media(max-width:760px){main{grid-template-columns:1fr}table{font-size:.82rem}th,td{padding:8px 4px}}
</style>
</head>
<body>
<header><h1>Paradise Bay — Administration</h1><p>Gérez les joueurs et récupérez une sauvegarde après une réinstallation.</p></header>
<main>
<section><div class="toolbar"><h2>Joueurs</h2><button class="secondary" id="refresh">Actualiser</button></div>
<div id="status" class="muted" role="status">Chargement…</div>
<table><thead><tr><th>Pseudo</th><th>Identifiant appareil</th><th>Fragments</th></tr></thead><tbody id="players"></tbody></table>
</section>
<section><h2>Sauvegarde sélectionnée</h2><div id="details" class="muted">Choisissez un joueur pour consulter sa sauvegarde.</div></section>
</main>
<script>
const playersEl=document.querySelector("#players"),detailsEl=document.querySelector("#details"),statusEl=document.querySelector("#status");
let selected="";
function esc(value){return String(value||"").replace(/[&<>"']/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]))}
async function request(url,options){const response=await fetch(url,options);const data=await response.json();if(!response.ok)throw new Error(data.error||"Erreur serveur");return data}
function setStatus(message,error=false){statusEl.textContent=message;statusEl.className=error?"notice error":"muted"}
async function refresh(){setStatus("Chargement…");try{const players=await request("/admin/api/players");playersEl.replaceChildren();if(!players.length){playersEl.innerHTML='<tr><td colspan="3" class="empty">Aucun joueur enregistré.</td></tr>'}
for(const p of players){const tr=document.createElement("tr");tr.className="player"+(p.uuid===selected?" selected":"");tr.innerHTML="<td>"+esc(p.alias)+"<div class=\"muted\">"+esc(p.uuid)+"</div></td><td>"+esc(p.z2did||"—")+"</td><td>"+p.fragmentCount+"</td>";tr.addEventListener("click",()=>showPlayer(p.uuid));playersEl.append(tr)}
setStatus(players.length+" joueur(s)");}catch(e){setStatus(e.message,true)}}
async function showPlayer(uuid){selected=uuid;await refresh();try{const data=await request("/admin/api/players/"+encodeURIComponent(uuid));renderDetails(data.player,data.fragments)}catch(e){detailsEl.textContent=e.message}}
function renderDetails(p,fragments){detailsEl.innerHTML='<div class="muted">UUID : '+esc(p.uuid)+'</div><div class="notice">'+p.fragmentCount+' fragment(s) · dernière sauvegarde : '+esc(p.lastSavedAt||"indisponible")+'<br>Solde : '+p.nanopods+' nanopods · '+p.gems+' gemmes</div><form id="identity"><label for="alias">Pseudo</label><input id="alias" maxlength="100" required><label for="z2did">Identifiant appareil (z2did)</label><input id="z2did" maxlength="256"><p class="muted">Après réinstallation, le jeu peut créer un compte « Guest ». Notez son z2did dans la liste, arrêtez le jeu, supprimez ce compte provisoire, puis rattachez son z2did à cette sauvegarde.</p><button>Enregistrer le rattachement</button></form><div class="actions"><a class="button secondary" href="/admin/api/players/'+encodeURIComponent(p.uuid)+'/backup">Télécharger la sauvegarde</a><button type="button" class="secondary" id="restore">Restaurer un fichier</button><button type="button" class="danger" id="deletePlayer">Supprimer ce compte</button><input id="backupFile" type="file" accept="application/json,.json" hidden></div><h2 style="margin-top:22px">Fragments enregistrés</h2><ul class="fragments" id="fragmentList"></ul>';
document.querySelector("#alias").value=p.alias;document.querySelector("#z2did").value=p.z2did||"";
const list=document.querySelector("#fragmentList");if(!fragments.length){list.innerHTML="<li>Aucun fragment enregistré.</li>"}else for(const f of fragments){const li=document.createElement("li");li.textContent=f.key+" · "+f.updatedAt;list.append(li)}
document.querySelector("#identity").addEventListener("submit",async e=>{e.preventDefault();try{await request("/admin/api/players/"+encodeURIComponent(p.uuid),{method:"PUT",headers:{"Content-Type":"application/json"},body:JSON.stringify({alias:document.querySelector("#alias").value,z2did:document.querySelector("#z2did").value})});setStatus("Joueur mis à jour.");await showPlayer(p.uuid)}catch(error){setStatus(error.message,true)}});
const skipTutorial=document.createElement("button");skipTutorial.type="button";skipTutorial.className="secondary";skipTutorial.textContent="Débloquer la caméra (sauter le tutoriel)";skipTutorial.addEventListener("click",async()=>{if(!confirm("Arrêtez le jeu et téléchargez d'abord une sauvegarde. Cette action marque comme terminées les dernières missions du tutoriel de plongée pour ce joueur. Continuer ?"))return;try{await request("/admin/api/players/"+encodeURIComponent(p.uuid)+"/skip-seal-house-tutorial",{method:"POST"});await showPlayer(p.uuid);setStatus("Tutoriel de plongée ignoré. Relancez le jeu pour appliquer le changement.")}catch(error){setStatus(error.message,true)}});document.querySelector(".actions").insertBefore(skipTutorial,document.querySelector("#deletePlayer"));
const fileInput=document.querySelector("#backupFile");document.querySelector("#restore").addEventListener("click",()=>fileInput.click());fileInput.addEventListener("change",async()=>{if(!fileInput.files.length)return;if(!confirm("La restauration remplacera les données actuelles de ce joueur. Continuer ?")){fileInput.value="";return}try{const backup=JSON.parse(await fileInput.files[0].text());await request("/admin/api/players/"+encodeURIComponent(p.uuid)+"/restore",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify(backup)});setStatus("Sauvegarde restaurée.");await showPlayer(p.uuid)}catch(error){setStatus(error.message,true)}finally{fileInput.value=""}});
document.querySelector("#deletePlayer").addEventListener("click",async()=>{if(!confirm("Supprimer définitivement "+p.alias+" et tous ses fragments ? Téléchargez une sauvegarde avant de continuer."))return;try{await request("/admin/api/players/"+encodeURIComponent(p.uuid),{method:"DELETE"});selected="";detailsEl.textContent="Compte supprimé.";setStatus("Compte et données associés supprimés.");await refresh()}catch(error){setStatus(error.message,true)}});
}
document.querySelector("#refresh").addEventListener("click",refresh);refresh();
</script>
</body>
</html>`
