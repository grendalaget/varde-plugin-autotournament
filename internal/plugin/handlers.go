package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/grendalaget/varde-plugin-autotournament/internal/at"
	"github.com/grendalaget/varde-plugin-autotournament/internal/state"
	"github.com/grendalaget/varde-plugin-autotournament/internal/varde"
)

const maxRequestBytes = 1 << 20

var mapPool = []string{"de_mirage", "de_inferno", "de_nuke"}

type envelope struct {
	GroupID string `json:"group_id"`
	User    struct {
		ID string `json:"id"`
	} `json:"user"`
	Settings map[string]json.RawMessage `json:"settings"`
	Values   map[string]json.RawMessage `json:"values"`
}

type listResponse struct {
	Items []listItem `json:"items"`
}

type listItem struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"`
	Status   string `json:"status"`
	Href     string `json:"href"`
}

type actionResponse struct {
	Message string   `json:"message"`
	Href    string   `json:"href,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

func (s *Service) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Varde-Op") != "settings" {
		writeError(w, http.StatusBadRequest, "unexpected plugin operation")
		return
	}
	request, claims, err := s.readEnvelope(w, r)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	if claims.Role != "admin" && claims.Role != "owner" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}
	atURL, err := rawString(request.Settings["at_url"])
	if err != nil || strings.TrimSpace(atURL) == "" {
		writeError(w, http.StatusBadRequest, "Auto Tournament URL is required")
		return
	}
	normalizedURL, err := at.NormalizeBaseURL(atURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	atToken, err := rawString(request.Settings["at_token"])
	if err != nil || strings.TrimSpace(atToken) == "" {
		writeError(w, http.StatusBadRequest, "Auto Tournament token is required")
		return
	}
	client, err := at.NewClientWithOptions(normalizedURL, atToken, s.allowPrivateAT)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := client.CheckToken(r.Context()); err != nil {
		if errors.Is(err, at.ErrUnauthorized) {
			writeError(w, http.StatusBadRequest, "Auto Tournament rejected the token")
			return
		}
		writeError(w, http.StatusBadGateway, "Auto Tournament is unreachable")
		return
	}

	previous, _ := s.state.Get(request.GroupID)
	fleetKey := ""
	if previous.ATURL == normalizedURL {
		fleetKey = previous.FleetKey
	}
	if fleetKey == "" {
		fleetKeyHint, hintErr := rawString(request.Settings["fleet_key"])
		if hintErr != nil {
			writeError(w, http.StatusBadRequest, "invalid fleet key")
			return
		}
		if fleetKeyHint != "" {
			fleetKeyURLHint, hintErr := rawString(request.Settings["fleet_key_url"])
			if hintErr == nil {
				fleetKeyURLHint, hintErr = at.NormalizeBaseURL(fleetKeyURLHint)
				if hintErr == nil && fleetKeyURLHint == normalizedURL {
					fleetKey = fleetKeyHint
				}
			}
		}
	}
	if fleetKey == "" {
		fleetKey, err = client.CreateFleetKey(r.Context(), "varde-"+request.GroupID)
		if err != nil {
			if errors.Is(err, at.ErrUnauthorized) {
				writeError(w, http.StatusBadRequest, "Auto Tournament rejected the token")
				return
			}
			writeError(w, http.StatusBadGateway, "Auto Tournament could not create a fleet key")
			return
		}
	}
	group := state.Group{
		ATURL:      normalizedURL,
		ATToken:    atToken,
		FleetKey:   fleetKey,
		AcceptedAt: previous.AcceptedAt,
	}
	if err := s.state.Save(request.GroupID, group); err != nil {
		s.logger.Error("save plugin group state", "group_id", request.GroupID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not save plugin settings")
		return
	}
	if s.readyUp != nil {
		s.enableGroup(request.GroupID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"store": map[string]string{
		"fleet_key": fleetKey, "fleet_key_url": normalizedURL,
	}})
}

func (s *Service) handleDisable(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Varde-Op") != "disable" {
		writeError(w, http.StatusBadRequest, "unexpected plugin operation")
		return
	}
	request, claims, err := s.readEnvelope(w, r)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	if claims.Role != "admin" && claims.Role != "owner" {
		writeError(w, http.StatusForbidden, "admin role required")
		return
	}
	if err := s.state.Delete(request.GroupID); err != nil {
		s.logger.Error("delete disabled plugin group state", "group_id", request.GroupID, "error", err)
		writeError(w, http.StatusInternalServerError, "could not disable plugin")
		return
	}
	if s.readyUp != nil {
		s.disableGroup(request.GroupID)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) handleTournaments(w http.ResponseWriter, r *http.Request) {
	request, _, err := s.readEnvelope(w, r)
	if err != nil {
		writeHandlerError(w, err)
		return
	}
	switch r.Header.Get("X-Varde-Op") {
	case "list":
		s.listTournaments(w, r.Context(), request.GroupID)
	case "action:create":
		s.createTournament(w, r.Context(), request)
	default:
		writeError(w, http.StatusBadRequest, "unexpected plugin operation")
	}
}

func (s *Service) listTournaments(w http.ResponseWriter, ctx context.Context, groupID string) {
	group, ok := s.state.Get(groupID)
	if !ok {
		writeError(w, http.StatusBadRequest, "Auto Tournament is not configured for this group")
		return
	}
	client, err := at.NewClientWithOptions(group.ATURL, group.ATToken, s.allowPrivateAT)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Auto Tournament settings are invalid")
		return
	}
	tournaments, err := client.ListTournaments(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Auto Tournament could not list tournaments")
		return
	}
	items := make([]listItem, 0, len(tournaments))
	for _, tournament := range tournaments {
		items = append(items, listItem{
			Title:    tournament.Name,
			Subtitle: tournament.Type + " · " + tournament.Status,
			Status:   tournament.Status,
			Href:     tournamentHref(group.ATURL, tournament.ID),
		})
	}
	writeJSON(w, http.StatusOK, listResponse{Items: items})
}

func (s *Service) createTournament(w http.ResponseWriter, ctx context.Context, request envelope) {
	group, ok := s.state.Get(request.GroupID)
	if !ok {
		writeError(w, http.StatusBadRequest, "Auto Tournament is not configured for this group")
		return
	}
	client, err := at.NewClientWithOptions(group.ATURL, group.ATToken, s.allowPrivateAT)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Auto Tournament settings are invalid")
		return
	}
	name, err := rawString(request.Values["name"])
	if err != nil || strings.TrimSpace(name) == "" {
		writeError(w, http.StatusBadRequest, "tournament name is required")
		return
	}
	format := "shuffle"
	if raw := request.Values["format"]; len(raw) > 0 {
		format, err = rawString(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid tournament format")
			return
		}
	}
	if format != "shuffle" && format != "single_elimination" && format != "double_elimination" {
		writeError(w, http.StatusBadRequest, "invalid tournament format")
		return
	}
	teamSize := 5
	if raw := request.Values["team_size"]; len(raw) > 0 {
		teamSize, err = rawInt(raw)
		if err != nil || teamSize < 2 || teamSize > 5 {
			writeError(w, http.StatusBadRequest, "team size must be 2–5")
			return
		}
	}
	maps := append([]string(nil), mapPool...)
	if raw := request.Values["maps"]; len(raw) > 0 {
		maps, err = rawStrings(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid map selection")
			return
		}
	}
	if len(maps) < 1 || len(maps) > 7 {
		writeError(w, http.StatusBadRequest, "maps must contain between 1 and 7 entries")
		return
	}
	allowedMaps := make(map[string]struct{}, len(mapPool))
	for _, gameMap := range mapPool {
		allowedMaps[gameMap] = struct{}{}
	}
	for _, gameMap := range maps {
		if _, ok := allowedMaps[gameMap]; !ok {
			writeError(w, http.StatusBadRequest, "map is not in the supported map pool")
			return
		}
	}
	seenMaps := make(map[string]struct{}, len(maps))
	for _, gameMap := range maps {
		if _, exists := seenMaps[gameMap]; exists {
			writeError(w, http.StatusBadRequest, "map selection cannot contain duplicates")
			return
		}
		seenMaps[gameMap] = struct{}{}
	}
	selected := []string(nil)
	if raw := request.Values["players"]; len(raw) > 0 {
		selected, err = rawMemberIDs(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid player selection")
			return
		}
	}

	members, err := s.varde.ListMembers(ctx, request.GroupID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Varde could not list group members")
		return
	}
	players, skipped, err := selectPlayers(members, selected)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(players) < 2*teamSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("need at least %d players with a Steam ID", 2*teamSize))
		return
	}
	if format != "shuffle" && len(players)%teamSize != 0 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("fixed-team tournament requires player count divisible by team_size (%d)", teamSize))
		return
	}

	imported := make([]at.Player, 0, len(players))
	steamIDs := make([]string, 0, len(players))
	for _, player := range players {
		imported = append(imported, at.Player{ID: player.steamID, Name: player.name})
		steamIDs = append(steamIDs, player.steamID)
	}
	importFailures, err := client.ImportPlayers(ctx, imported)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Auto Tournament could not import players")
		return
	}
	if len(importFailures) > 0 {
		writeError(w, http.StatusBadGateway, "Auto Tournament rejected one or more players")
		return
	}

	var tournament at.Tournament
	var registrationFailures []at.PlayerFailure
	if format == "shuffle" {
		tournament, err = client.CreateShuffle(ctx, name, teamSize, maps)
		if err == nil {
			registrationFailures, err = client.RegisterPlayers(ctx, tournament.ID, steamIDs)
		}
	} else {
		if err := shuffleTournamentPlayers(players); err != nil {
			writeError(w, http.StatusInternalServerError, "could not assign tournament teams")
			return
		}
		suffix, err := randomTeamSuffix()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not assign tournament teams")
			return
		}
		teams, teamIDs := chunkTournamentTeams(players, teamSize, suffix)
		if err := client.CreateTeams(ctx, teams); err != nil {
			writeError(w, http.StatusBadGateway, "Auto Tournament could not create teams")
			return
		}
		tournament, err = client.CreateBracket(ctx, name, format, "bo1", teamSize, maps, teamIDs)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "Auto Tournament could not create the tournament")
		return
	}
	registrationNotes := make([]string, 0, len(registrationFailures))
	for _, failure := range registrationFailures {
		if failure.Name != "" {
			registrationNotes = appendUnique(registrationNotes, failure.Name)
		} else if failure.ID != "" {
			registrationNotes = appendUnique(registrationNotes, failure.ID)
		}
	}
	notes := make([]string, 0, len(skipped)+len(registrationNotes)+1)
	if len(skipped) > 0 {
		notes = append(notes, "Skipped members without a Steam ID: "+strings.Join(skipped, ", "))
	}
	if len(registrationNotes) > 0 {
		notes = append(notes, "Could not register players: "+strings.Join(registrationNotes, ", "))
	}
	if err := client.Start(ctx, tournament.ID); err != nil {
		writeError(w, http.StatusBadGateway, "Auto Tournament could not start the tournament")
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{
		Message: "Tournament created: " + name,
		Href:    tournamentHref(group.ATURL, tournament.ID),
		Notes:   notes,
	})
}

type tournamentPlayer struct {
	steamID string
	name    string
}

func selectPlayers(members []varde.Member, selected []string) ([]tournamentPlayer, []string, error) {
	byID := make(map[string]varde.Member, len(members))
	for _, member := range members {
		byID[member.UserID] = member
	}
	chosen := make([]varde.Member, 0, len(members))
	if len(selected) == 0 {
		chosen = members
	} else {
		seen := make(map[string]struct{}, len(selected))
		for _, userID := range selected {
			if _, exists := seen[userID]; exists {
				continue
			}
			seen[userID] = struct{}{}
			member, ok := byID[userID]
			if !ok {
				return nil, nil, errors.New("selected user is not a group member")
			}
			chosen = append(chosen, member)
		}
	}
	players := make([]tournamentPlayer, 0, len(chosen))
	skipped := make([]string, 0)
	missing := make([]string, 0)
	for _, member := range chosen {
		if member.SteamID == nil || *member.SteamID == "" {
			if len(selected) > 0 {
				missing = append(missing, member.DisplayName)
			} else {
				skipped = append(skipped, member.DisplayName)
			}
			continue
		}
		players = append(players, tournamentPlayer{steamID: *member.SteamID, name: member.DisplayName})
	}
	if len(missing) > 0 {
		return nil, nil, errors.New("missing Steam ID for: " + strings.Join(missing, ", "))
	}
	return players, skipped, nil
}

func shuffleTournamentPlayers(players []tournamentPlayer) error {
	for i := len(players) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return fmt.Errorf("shuffle tournament players: %w", err)
		}
		j := int(n.Int64())
		players[i], players[j] = players[j], players[i]
	}
	return nil
}

func randomTeamSuffix() (string, error) {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate team IDs: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func chunkTournamentTeams(players []tournamentPlayer, teamSize int, suffix string) ([]at.Team, []string) {
	teams := make([]at.Team, 0, len(players)/teamSize)
	teamIDs := make([]string, 0, len(players)/teamSize)
	for start := 0; start < len(players); start += teamSize {
		n := len(teams) + 1
		id := fmt.Sprintf("varde-%s-%d", suffix, n)
		teamIDs = append(teamIDs, id)
		roster := make([]at.TeamPlayer, 0, teamSize)
		for _, player := range players[start : start+teamSize] {
			roster = append(roster, at.TeamPlayer{SteamID: player.steamID, Name: player.name})
		}
		teams = append(teams, at.Team{
			ID: id, Name: fmt.Sprintf("Team %d", n), Tag: fmt.Sprintf("T%d", n), Players: roster,
		})
	}
	return teams, teamIDs
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func tournamentHref(baseURL, id string) string {
	return strings.TrimRight(baseURL, "/") + "/tournament/" + url.PathEscape(id)
}

func (s *Service) readEnvelope(w http.ResponseWriter, r *http.Request) (envelope, Claims, error) {
	var request envelope
	reader := http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&request); err != nil {
		return envelope{}, Claims{}, fmt.Errorf("invalid request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return envelope{}, Claims{}, errors.New("request must contain one JSON object")
	}
	claims, err := verifyToken(s.secret, r.Header.Get("Authorization"))
	if err != nil {
		return envelope{}, Claims{}, err
	}
	if err := s.validateGroup(request.GroupID); err != nil {
		return envelope{}, Claims{}, err
	}
	if claims.GroupID != request.GroupID {
		return envelope{}, Claims{}, errors.New("token group does not match request group")
	}
	if claims.Subject != request.User.ID {
		return envelope{}, Claims{}, errors.New("token user does not match request user")
	}
	return request, claims, nil
}

func rawString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func rawInt(raw json.RawMessage) (int, error) {
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, err
	}
	parsed, err := value.Int64()
	if err != nil {
		return 0, err
	}
	return int(parsed), nil
}

func rawStrings(raw json.RawMessage) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func rawMemberIDs(raw json.RawMessage) ([]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, errors.New("members must be an array")
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(values))
	for index, value := range values {
		item := strings.TrimSpace(string(value))
		var userID string
		switch {
		case len(item) > 0 && item[0] == '{':
			var member map[string]json.RawMessage
			if err := json.Unmarshal(value, &member); err != nil {
				return nil, err
			}
			rawUserID, ok := member["user_id"]
			if !ok {
				return nil, fmt.Errorf("member %d has no user_id", index)
			}
			if strings.TrimSpace(string(rawUserID)) == "null" {
				return nil, fmt.Errorf("member %d has an invalid user_id", index)
			}
			if err := json.Unmarshal(rawUserID, &userID); err != nil {
				return nil, fmt.Errorf("member %d has an invalid user_id: %w", index, err)
			}
		case len(item) > 0 && item[0] == '"':
			if err := json.Unmarshal(value, &userID); err != nil {
				return nil, fmt.Errorf("member %d has an invalid user ID: %w", index, err)
			}
		default:
			return nil, fmt.Errorf("member %d must be an object or string", index)
		}
		if strings.TrimSpace(userID) != "" {
			ids = append(ids, userID)
		}
	}
	return ids, nil
}

func writeHandlerError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if err.Error() == "invalid bearer token" || err.Error() == "missing bearer token" {
		status = http.StatusUnauthorized
	}
	writeError(w, status, err.Error())
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
