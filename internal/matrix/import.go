package matrix

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aligundogdu/matrixmigrate/internal/logger"
	"github.com/aligundogdu/matrixmigrate/internal/mattermost"
)

// Importer handles importing data to Matrix
type Importer struct {
	client *Client
}

// NewImporter creates a new importer
func NewImporter(client *Client) *Importer {
	return &Importer{client: client}
}

// ImportProgressCallback is called to report import progress
type ImportProgressCallback func(stage string, current, total int, item string)

// trySendMessage sends a message as senderID, falling back to AS bot identity
// if Synapse reports the user has not been registered with the AS.
func (i *Importer) trySendMessage(roomID, message string, timestamp int64, senderID string) (*SendMessageResponse, error) {
	resp, err := i.client.SendMessageWithTimestamp(roomID, message, timestamp, senderID)
	if err != nil && senderID != "" && strings.Contains(err.Error(), "has not registered this user") {
		logger.Warn("AS impersonation failed for %s, retrying as AS bot", senderID)
		resp, err = i.client.SendMessageWithTimestamp(roomID, message, timestamp, "")
	}
	return resp, err
}

// trySendReply sends a reply as senderID, falling back to AS bot identity
// if Synapse reports the user has not been registered with the AS.
func (i *Importer) trySendReply(roomID, message, parentEventID string, timestamp int64, senderID string) (*SendMessageResponse, error) {
	resp, err := i.client.SendReplyWithTimestamp(roomID, message, parentEventID, timestamp, senderID)
	if err != nil && senderID != "" && strings.Contains(err.Error(), "has not registered this user") {
		logger.Warn("AS impersonation failed for %s, retrying as AS bot", senderID)
		resp, err = i.client.SendReplyWithTimestamp(roomID, message, parentEventID, timestamp, "")
	}
	return resp, err
}

// preRegisterASUsers registers all Matrix user IDs in userMapping with the AS
// so that Synapse allows ?user_id= impersonation for each of them.
func (i *Importer) preRegisterASUsers(userMapping map[string]string) {
	if !i.client.HasASToken() {
		return
	}
	seen := make(map[string]bool)
	for _, mxUserID := range userMapping {
		if mxUserID == "" || seen[mxUserID] {
			continue
		}
		seen[mxUserID] = true
		if err := i.client.RegisterASUser(mxUserID); err != nil {
			logger.Warn("AS pre-registration failed for %s: %v", mxUserID, err)
		}
	}
	logger.Info("AS pre-registration complete: %d users registered", len(seen))
}

// GenerateRandomPassword generates a random password for new users
func GenerateRandomPassword() string {
	// In production, use crypto/rand for secure random password
	return "ChangeMe123!" // Placeholder - users should change this
}

// ImportUsers imports users from Mattermost to Matrix
func (i *Importer) ImportUsers(users []mattermost.User, existingMapping map[string]string, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(users)

	logger.Info("Starting user import: %d users to process", total)

	// Copy existing mappings
	for k, v := range existingMapping {
		mapping[k] = v
	}
	logger.Info("Existing mappings copied: %d entries", len(existingMapping))

	for idx, user := range users {
		logger.Info("Processing user %d/%d: %s (ID: %s)", idx+1, total, user.Username, user.ID)
		
		if progress != nil {
			progress("users", idx+1, total, user.Username)
		}

		// Skip deleted users
		if user.IsDeleted() {
			logger.Info("User '%s' is deleted, skipping", user.Username)
			stats.UsersSkipped++
			continue
		}

		// Skip if already in mapping
		if _, exists := existingMapping[user.ID]; exists {
			logger.Info("User '%s' already in mapping, skipping", user.Username)
			stats.UsersSkipped++
			continue
		}

		// Try to check if user exists, but don't fail if check fails
		// (some Matrix servers only allow checking local users)
		exists := false
		existsCheck, err := i.client.UserExists(user.Username)
		if err != nil {
			// If check fails with "Can only look up local users", ignore it
			// CreateUser is idempotent anyway, so we can just try to create
			if strings.Contains(err.Error(), "Can only look up local users") {
				logger.Info("UserExists check not available for '%s', will try to create", user.Username)
			} else {
				logger.Warn("UserExists check failed for '%s': %v, will try to create anyway", user.Username, err)
			}
		} else {
			exists = existsCheck
		}

		if exists {
			// User already exists, just add to mapping
			mapping[user.ID] = i.client.FormatUserID(user.Username)
			logger.Info("User '%s' already exists, skipped", user.Username)
			stats.UsersSkipped++
			continue
		}

		// Create the user (CreateUser is idempotent - if user exists, it will update)
		displayName := strings.TrimSpace(user.FirstName + " " + user.LastName)
		if displayName == "" {
			displayName = user.Username
		}

		req := &CreateUserRequest{
			Password:    GenerateRandomPassword(),
			DisplayName: displayName,
			Admin:       false,
			Deactivated: false,
		}

		resp, err := i.client.CreateUser(user.Username, req)
		if err != nil {
			// Check if error is because user already exists
			if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "M_USER_IN_USE") {
				// User exists, add to mapping
				mapping[user.ID] = i.client.FormatUserID(user.Username)
				logger.Info("User '%s' already exists (detected during create), skipped", user.Username)
				stats.UsersSkipped++
				continue
			}
			logger.Error("Failed to create user '%s': %v", user.Username, err)
			stats.UsersFailed++
			continue
		}
		logger.Success("Created user '%s' -> %s", user.Username, resp.UserID)

		mapping[user.ID] = resp.UserID
		stats.UsersCreated++
	}

	return mapping, stats, nil
}

// ImportTeamsAsSpaces imports teams from Mattermost as Matrix spaces
func (i *Importer) ImportTeamsAsSpaces(teams []mattermost.Team, existingMapping map[string]string, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(teams)

	// Copy existing mappings
	for k, v := range existingMapping {
		mapping[k] = v
	}

	for idx, team := range teams {
		if progress != nil {
			progress("spaces", idx+1, total, team.DisplayName)
		}

		// Skip deleted teams
		if team.IsDeleted() {
			stats.SpacesSkipped++
			continue
		}

		// Skip if already imported (exists in mapping)
		if _, exists := existingMapping[team.ID]; exists {
			logger.Info("Space '%s' already imported, skipped", team.DisplayName)
			stats.SpacesSkipped++
			continue
		}

		// Create space
		resp, err := i.client.CreateSpace(team.DisplayName, team.Description, team.IsOpen())
		if err != nil {
			logger.Error("Failed to create space '%s': %v", team.DisplayName, err)
			stats.SpacesFailed++
			continue
		}

		logger.Success("Created space '%s' -> %s", team.DisplayName, resp.RoomID)
		mapping[team.ID] = resp.RoomID
		stats.SpacesCreated++
	}

	return mapping, stats, nil
}

// ImportChannelsAsRooms imports channels from Mattermost as Matrix rooms
func (i *Importer) ImportChannelsAsRooms(channels []mattermost.Channel, userMapping map[string]string, existingMapping map[string]string, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	return i.ImportChannelsAsRoomsWithDMs(channels, userMapping, existingMapping, true, progress)
}

// ImportChannelsAsRoomsWithDMs imports channels from Mattermost as Matrix rooms with optional DM support
func (i *Importer) ImportChannelsAsRoomsWithDMs(channels []mattermost.Channel, userMapping map[string]string, existingMapping map[string]string, migrateDMs bool, progress ImportProgressCallback) (map[string]string, *ImportStats, error) {
	mapping := make(map[string]string)
	stats := &ImportStats{}
	total := len(channels)

	// Copy existing mappings
	for k, v := range existingMapping {
		mapping[k] = v
	}

	for idx, channel := range channels {
		if progress != nil {
			progress("rooms", idx+1, total, channel.DisplayName)
		}

		// Skip deleted channels
		if channel.IsDeleted() {
			stats.RoomsSkipped++
			continue
		}

		// Handle direct messages
		if channel.IsDirect() {
			if !migrateDMs {
				stats.DMRoomsSkipped++
				continue
			}
			roomID, err := i.importDMAsRoom(channel, userMapping, existingMapping)
			if err != nil {
				logger.Warn("Failed to import DM: %v", err)
				stats.DMRoomsFailed++
			} else if roomID != "" {
				mapping[channel.ID] = roomID
				stats.DMRoomsCreated++
				logger.Success("Created DM room -> %s", roomID)
			} else {
				stats.DMRoomsSkipped++
			}
			continue
		}

		// Skip if already imported (exists in mapping)
		if _, exists := existingMapping[channel.ID]; exists {
			logger.Info("Room '%s' already imported, skipped", channel.DisplayName)
			stats.RoomsSkipped++
			continue
		}

		// Create room
		topic := channel.Purpose
		if topic == "" {
			topic = channel.Header
		}

		resp, err := i.client.CreateRegularRoom(channel.DisplayName, topic, channel.IsPublic())
		if err != nil {
			logger.Error("Failed to create room '%s': %v", channel.DisplayName, err)
			stats.RoomsFailed++
			continue
		}

		logger.Success("Created room '%s' -> %s", channel.DisplayName, resp.RoomID)
		mapping[channel.ID] = resp.RoomID
		stats.RoomsCreated++
	}

	return mapping, stats, nil
}

// importDMAsRoom imports a Mattermost DM channel as a Matrix DM room
// Returns the room ID if successful, empty string if skipped, error if failed
func (i *Importer) importDMAsRoom(channel mattermost.Channel, userMapping map[string]string, existingMapping map[string]string) (string, error) {
	// Skip if already imported
	if roomID, exists := existingMapping[channel.ID]; exists {
		logger.Info("DM room already imported, skipped")
		return roomID, nil
	}

	// Parse DM user IDs
	mmUserA, mmUserB, ok := channel.DMUserIDs()
	if !ok {
		return "", fmt.Errorf("invalid DM channel name format: %s", channel.Name)
	}

	// Skip self-DMs
	if mmUserA == mmUserB {
		logger.Info("Skipping self-DM for user %s", mmUserA)
		return "", nil
	}

	// Look up both users in mapping
	mxUserA, existsA := userMapping[mmUserA]
	mxUserB, existsB := userMapping[mmUserB]

	if !existsA || !existsB {
		if !existsA {
			logger.Warn("DM user %s not in mapping, skipping DM", mmUserA)
		}
		if !existsB {
			logger.Warn("DM user %s not in mapping, skipping DM", mmUserB)
		}
		return "", nil
	}

	// Create DM room with both users invited
	resp, err := i.client.CreateDMRoom([]string{mxUserA, mxUserB})
	if err != nil {
		return "", fmt.Errorf("failed to create DM room: %w", err)
	}

	logger.Info("Created DM room %s for users %s and %s", resp.RoomID, mxUserA, mxUserB)

	// Force-join both users using Admin API (no invitation acceptance required)
	if err := i.client.ForceJoinUser(resp.RoomID, mxUserA); err != nil {
		logger.Warn("Failed to force-join %s to DM: %v", mxUserA, err)
		// Continue even if one user fails to join
	}
	if err := i.client.ForceJoinUser(resp.RoomID, mxUserB); err != nil {
		logger.Warn("Failed to force-join %s to DM: %v", mxUserB, err)
		// Continue even if one user fails to join
	}

	// Remove admin user from DM room (DMs should only contain the two participants)
	if err := i.client.LeaveRoom(resp.RoomID); err != nil {
		logger.Warn("Failed to remove admin from DM room: %v", err)
		// Non-critical error - room is still functional
	}

	return resp.RoomID, nil
}

// ApplyTeamMemberships invites users to spaces based on team memberships
func (i *Importer) ApplyTeamMemberships(
	memberships []mattermost.TeamMember,
	userMapping map[string]string,
	spaceMapping map[string]string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(memberships)

	logger.Info("Starting team membership import: %d memberships to process", total)

	for idx, membership := range memberships {
		if progress != nil {
			progress("team_memberships", idx+1, total, "")
		}

		// Skip deleted memberships
		if membership.IsDeleted() {
			logger.Info("Team membership %d/%d: deleted, skipping", idx+1, total)
			stats.MembersSkipped++
			continue
		}

		// Get Matrix IDs
		userID, userExists := userMapping[membership.UserID]
		spaceID, spaceExists := spaceMapping[membership.TeamID]

		if !userExists || !spaceExists {
			if !userExists {
				logger.Warn("Team membership %d/%d skipped: user %s not in mapping", idx+1, total, membership.UserID)
			}
			if !spaceExists {
				logger.Warn("Team membership %d/%d skipped: team %s not in mapping", idx+1, total, membership.TeamID)
			}
			stats.MembersSkipped++
			continue
		}

		logger.Info("Team membership %d/%d: inviting %s to space %s", idx+1, total, userID, spaceID)

		// Invite user to space
		if err := i.client.InviteUser(spaceID, userID); err != nil {
			logger.Error("Team membership %d/%d failed: %s -> %s: %v", idx+1, total, userID, spaceID, err)
			stats.MembersFailed++
			continue
		}

		logger.Success("Team membership %d/%d: %s added to space", idx+1, total, userID)
		stats.MembersAdded++
	}

	logger.Info("Team membership import completed: added=%d, skipped=%d, failed=%d", 
		stats.MembersAdded, stats.MembersSkipped, stats.MembersFailed)

	return stats, nil
}

// ApplyChannelMemberships invites users to rooms based on channel memberships
func (i *Importer) ApplyChannelMemberships(
	memberships []mattermost.ChannelMember,
	userMapping map[string]string,
	roomMapping map[string]string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(memberships)

	logger.Info("Starting channel membership import: %d memberships to process", total)

	for idx, membership := range memberships {
		if progress != nil {
			progress("channel_memberships", idx+1, total, "")
		}

		// Get Matrix IDs
		userID, userExists := userMapping[membership.UserID]
		roomID, roomExists := roomMapping[membership.ChannelID]

		if !userExists || !roomExists {
			if !userExists {
				logger.Warn("Channel membership %d/%d skipped: user %s not in mapping", idx+1, total, membership.UserID)
			}
			if !roomExists {
				logger.Warn("Channel membership %d/%d skipped: channel %s not in mapping", idx+1, total, membership.ChannelID)
			}
			stats.MembersSkipped++
			continue
		}

		logger.Info("Channel membership %d/%d: inviting %s to room %s", idx+1, total, userID, roomID)

		// Invite user to room
		if err := i.client.InviteUser(roomID, userID); err != nil {
			logger.Error("Channel membership %d/%d failed: %s -> %s: %v", idx+1, total, userID, roomID, err)
			stats.MembersFailed++
			continue
		}

		logger.Success("Channel membership %d/%d: %s added to room", idx+1, total, userID)
		stats.MembersAdded++
	}

	logger.Info("Channel membership import completed: added=%d, skipped=%d, failed=%d", 
		stats.MembersAdded, stats.MembersSkipped, stats.MembersFailed)

	return stats, nil
}

// LinkRoomsToSpaces links rooms to their parent spaces based on channel-team relationships
func (i *Importer) LinkRoomsToSpaces(
	channels []mattermost.Channel,
	spaceMapping map[string]string,
	roomMapping map[string]string,
	progress ImportProgressCallback,
) (*ImportStats, error) {
	stats := &ImportStats{}
	total := len(channels)

	for idx, channel := range channels {
		if progress != nil {
			progress("linking", idx+1, total, channel.DisplayName)
		}

		// Skip if no team association
		if channel.TeamID == "" {
			continue
		}

		// Get Matrix IDs
		spaceID, spaceExists := spaceMapping[channel.TeamID]
		roomID, roomExists := roomMapping[channel.ID]

		if !spaceExists || !roomExists {
			continue
		}

		// Add room as child of space
		if err := i.client.AddRoomToSpace(spaceID, roomID, true); err != nil {
			logger.Error("Failed to link room '%s' to space: %v", channel.DisplayName, err)
			stats.RoomsLinkFailed++
			continue
		}

		// Set space as parent of room
		if err := i.client.SetRoomParent(roomID, spaceID, true); err != nil {
			// Non-critical error, room is still linked as child
			logger.Warn("Failed to set parent for room '%s': %v", channel.DisplayName, err)
		}

		logger.Success("Linked room '%s' to space", channel.DisplayName)
		stats.RoomsLinked++
	}

	return stats, nil
}

// ImportAssetsResult holds the result of importing assets
type ImportAssetsResult struct {
	UserMapping  map[string]string
	SpaceMapping map[string]string
	RoomMapping  map[string]string
	Stats        *ImportStats
}

// ExistingMappings holds existing mappings to skip already imported items
type ExistingMappings struct {
	Users    map[string]string
	Spaces   map[string]string
	Rooms    map[string]string
}

// ImportAssets imports all assets (users, teams as spaces, channels as rooms)
// If existingMappings is provided, already imported items will be skipped
func (i *Importer) ImportAssets(assets *mattermost.Assets, existingMappings *ExistingMappings, progress ImportProgressCallback) (*ImportAssetsResult, error) {
	return i.ImportAssetsWithDMs(assets, existingMappings, true, progress)
}

// ImportAssetsWithDMs imports all assets with optional DM support
func (i *Importer) ImportAssetsWithDMs(assets *mattermost.Assets, existingMappings *ExistingMappings, migrateDMs bool, progress ImportProgressCallback) (*ImportAssetsResult, error) {
	result := &ImportAssetsResult{
		Stats: &ImportStats{},
	}

	logger.Info("=== ImportAssets Started ===")
	logger.Info("Assets to import: %d users, %d teams, %d channels",
		len(assets.Users), len(assets.Teams), len(assets.Channels))

	// Initialize empty mappings if not provided
	if existingMappings == nil {
		existingMappings = &ExistingMappings{
			Users:  make(map[string]string),
			Spaces: make(map[string]string),
			Rooms:  make(map[string]string),
		}
		logger.Info("No existing mappings provided, starting fresh")
	} else {
		logger.Info("Existing mappings: %d users, %d spaces, %d rooms",
			len(existingMappings.Users), len(existingMappings.Spaces), len(existingMappings.Rooms))
	}

	// Import users
	logger.Info("=== Starting User Import ===")
	userMapping, userStats, err := i.ImportUsers(assets.Users, existingMappings.Users, progress)
	if err != nil {
		logger.Error("User import failed: %v", err)
		return nil, fmt.Errorf("failed to import users: %w", err)
	}
	result.UserMapping = userMapping
	result.Stats.UsersCreated = userStats.UsersCreated
	result.Stats.UsersSkipped = userStats.UsersSkipped
	result.Stats.UsersFailed = userStats.UsersFailed
	logger.Info("User import completed: created=%d, skipped=%d, failed=%d",
		userStats.UsersCreated, userStats.UsersSkipped, userStats.UsersFailed)

	// Import teams as spaces
	spaceMapping, spaceStats, err := i.ImportTeamsAsSpaces(assets.Teams, existingMappings.Spaces, progress)
	if err != nil {
		return nil, fmt.Errorf("failed to import teams: %w", err)
	}
	result.SpaceMapping = spaceMapping
	result.Stats.SpacesCreated = spaceStats.SpacesCreated
	result.Stats.SpacesSkipped = spaceStats.SpacesSkipped
	result.Stats.SpacesFailed = spaceStats.SpacesFailed

	// Import channels as rooms
	roomMapping, roomStats, err := i.ImportChannelsAsRoomsWithDMs(assets.Channels, userMapping, existingMappings.Rooms, migrateDMs, progress)
	if err != nil {
		return nil, fmt.Errorf("failed to import channels: %w", err)
	}
	result.RoomMapping = roomMapping
	result.Stats.RoomsCreated = roomStats.RoomsCreated
	result.Stats.RoomsSkipped = roomStats.RoomsSkipped
	result.Stats.RoomsFailed = roomStats.RoomsFailed
	result.Stats.DMRoomsCreated = roomStats.DMRoomsCreated
	result.Stats.DMRoomsSkipped = roomStats.DMRoomsSkipped
	result.Stats.DMRoomsFailed = roomStats.DMRoomsFailed

	return result, nil
}

// MessageImportStats holds statistics about message import
type MessageImportStats struct {
	MessagesImported int `json:"messages_imported"`
	MessagesSkipped  int `json:"messages_skipped"` // Already imported
	MessagesFailed   int `json:"messages_failed"`
	RepliesImported  int `json:"replies_imported"`
	RepliesFailed    int `json:"replies_failed"`  // Reply target not found
	FilesLinked      int `json:"files_linked"`    // Files added as links
	FilesUploaded    int `json:"files_uploaded"`  // Files uploaded to Matrix
	FilesSkipped     int `json:"files_skipped"`   // Files skipped
}

// FileConfig holds file migration settings
type FileConfig struct {
	// Mode controls how file attachments are handled:
	//   "link"   – embed S3/public URL as a markdown link in the message
	//   "local"  – read file from LocalDataPath and upload to Matrix media API (local mode only)
	//   "upload" – (reserved, not yet implemented)
	//   "skip"   – ignore all file attachments
	Mode          string
	S3PublicURL   string // Base URL for S3 link mode
	MaxUploadSize int64  // Max bytes to upload in local/upload mode (0 = no limit)
	LocalDataPath string // Absolute path to Mattermost data directory (local mode only)
}

// MessageImportCallback is called for each message imported
type MessageImportCallback func(current, total int, channelName string, status string)

// ImportMessagesResult contains the result of message import
type ImportMessagesResult struct {
	Stats    *MessageImportStats
	Mapping  map[string]string // MattermostID -> MatrixEventID
	Errors   []string
}

// ImportMessages imports messages from Mattermost posts to Matrix rooms
// This requires Application Service token for timestamp support
func (i *Importer) ImportMessages(
	posts []mattermost.Post,
	channelToRoom map[string]string,      // Mattermost channel ID -> Matrix room ID
	userMapping map[string]string,         // Mattermost user ID -> Matrix user ID
	existingMapping map[string]string,     // Mattermost post ID -> Matrix event ID (for resume)
	progress MessageImportCallback,
) (*ImportMessagesResult, error) {
	result := &ImportMessagesResult{
		Stats:   &MessageImportStats{},
		Mapping: make(map[string]string),
		Errors:  []string{},
	}
	
	if !i.client.HasASToken() {
		logger.Warn("No Application Service token configured - messages will be imported without original timestamps")
	}
	
	total := len(posts)
	logger.Info("Starting message import: %d posts to process", total)

	// Pre-register all users with the AS so Synapse allows ?user_id= impersonation.
	i.preRegisterASUsers(userMapping)

	// Collect all existing mappings
	for k, v := range existingMapping {
		result.Mapping[k] = v
	}

	// Process messages in order
	for idx, post := range posts {
		// Check if already imported
		if _, exists := existingMapping[post.ID]; exists {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped")
			}
			continue
		}
		
		// Get target room
		roomID, roomExists := channelToRoom[post.ChannelID]
		if !roomExists {
			result.Stats.MessagesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("No room mapping for channel %s (post %s)", post.ChannelID, post.ID))
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "failed:no_room")
			}
			continue
		}
		
		// Get sender
		senderID, userExists := userMapping[post.UserID]
		if !userExists {
			// Fall back to empty sender (will use AS bot)
			senderID = ""
			logger.Warn("No user mapping for user %s, message will be sent as AS bot", post.UserID)
		}
		
		// Handle reply
		var eventID string
		
		if post.IsReply() {
			parentEventID, parentExists := result.Mapping[post.RootID]
			if !parentExists {
				result.Stats.RepliesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Parent post %s not found for reply %s", post.RootID, post.ID))

				resp, sendErr := i.trySendMessage(roomID, post.Message, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.MessagesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:send_error")
					}
					continue
				}
				eventID = resp.EventID
			} else {
				resp, sendErr := i.trySendReply(roomID, post.Message, parentEventID, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.RepliesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send reply %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:reply_error")
					}
					continue
				}
				eventID = resp.EventID
				result.Stats.RepliesImported++
			}
		} else {
			resp, sendErr := i.trySendMessage(roomID, post.Message, post.CreateAt, senderID)
			if sendErr != nil {
				result.Stats.MessagesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
				if progress != nil {
					progress(idx+1, total, post.ChannelID, "failed:send_error")
				}
				continue
			}
			eventID = resp.EventID
		}

		result.Mapping[post.ID] = eventID
		result.Stats.MessagesImported++

		if progress != nil {
			progress(idx+1, total, post.ChannelID, "imported")
		}

		if (idx+1)%100 == 0 {
			logger.Info("Message import progress: %d/%d (%.1f%%)", idx+1, total, float64(idx+1)/float64(total)*100)
		}
	}

	logger.Info("Message import completed: imported=%d, skipped=%d, failed=%d, replies=%d",
		result.Stats.MessagesImported, result.Stats.MessagesSkipped,
		result.Stats.MessagesFailed, result.Stats.RepliesImported)

	return result, nil
}

// uploadLocalFile reads a file from the Mattermost data directory and uploads it to
// the Matrix media API, then sends it as a native file event in the room.
// file.Path is always forward-slash separated (Mattermost convention); filepath.FromSlash
// converts it to the OS-native separator so this works on both Linux and Windows.
func (i *Importer) uploadLocalFile(
	roomID string,
	file mattermost.FileInfo,
	localDataPath string,
	maxSize int64,
	timestamp int64,
	senderID string,
) error {
	if maxSize > 0 && file.Size > maxSize {
		return fmt.Errorf("file too large (%d bytes, limit %d)", file.Size, maxSize)
	}

	localPath := filepath.Join(localDataPath, filepath.FromSlash(file.Path))

	data, err := os.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", localPath, err)
	}

	mimeType := file.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	uploadResp, err := i.client.UploadMedia(data, file.Name, mimeType)
	if err != nil {
		return fmt.Errorf("Matrix upload: %w", err)
	}

	_, err = i.client.SendUploadedFile(
		roomID, uploadResp.ContentURI,
		file.Name, mimeType, file.Size,
		file.Width, file.Height,
		timestamp, senderID,
	)
	if err != nil {
		return fmt.Errorf("send file event: %w", err)
	}

	return nil
}

// ImportMessagesWithFiles imports messages with file attachments
// filesByPost maps post ID to list of file infos
func (i *Importer) ImportMessagesWithFiles(
	posts []mattermost.Post,
	channelToRoom map[string]string,
	userMapping map[string]string,
	existingMapping map[string]string,
	filesByPost map[string][]mattermost.FileInfo,
	fileConfig *FileConfig,
	progress MessageImportCallback,
) (*ImportMessagesResult, error) {
	result := &ImportMessagesResult{
		Stats:   &MessageImportStats{},
		Mapping: make(map[string]string),
		Errors:  []string{},
	}
	
	if !i.client.HasASToken() {
		logger.Warn("No Application Service token configured - messages will be imported without original timestamps")
	}
	
	total := len(posts)
	logger.Info("Starting message import with files: %d posts to process", total)

	// Pre-register all users with the AS so Synapse allows ?user_id= impersonation.
	i.preRegisterASUsers(userMapping)

	// Default file config
	if fileConfig == nil {
		fileConfig = &FileConfig{Mode: "skip"}
	}

	// Collect all existing mappings
	for k, v := range existingMapping {
		result.Mapping[k] = v
	}

	// Process messages in order
	for idx, post := range posts {
		// Check if already imported
		if _, exists := existingMapping[post.ID]; exists {
			result.Stats.MessagesSkipped++
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "skipped")
			}
			continue
		}
		
		// Get target room
		roomID, roomExists := channelToRoom[post.ChannelID]
		if !roomExists {
			result.Stats.MessagesFailed++
			result.Errors = append(result.Errors, fmt.Sprintf("No room mapping for channel %s (post %s)", post.ChannelID, post.ID))
			if progress != nil {
				progress(idx+1, total, post.ChannelID, "failed:no_room")
			}
			continue
		}
		
		// Get sender
		senderID, userExists := userMapping[post.UserID]
		if !userExists {
			senderID = ""
			logger.Warn("No user mapping for user %s, message will be sent as AS bot", post.UserID)
		}
		
		// Build message content with files
		messageContent := post.Message
		files := filesByPost[post.ID]
		
		// Append file links if mode is "link"
		if fileConfig.Mode == "link" && len(files) > 0 && fileConfig.S3PublicURL != "" {
			for _, file := range files {
				fileURL := strings.TrimSuffix(fileConfig.S3PublicURL, "/") + "/" + file.Path
				messageContent += fmt.Sprintf("\n\n📎 [%s](%s)", file.Name, fileURL)
				result.Stats.FilesLinked++
			}
		}
		
		// Handle reply
		var eventID string
		
		if post.IsReply() {
			parentEventID, parentExists := result.Mapping[post.RootID]
			if !parentExists {
				result.Stats.RepliesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Parent post %s not found for reply %s", post.RootID, post.ID))

				resp, sendErr := i.trySendMessage(roomID, messageContent, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.MessagesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:send_error")
					}
					continue
				}
				eventID = resp.EventID
			} else {
				resp, sendErr := i.trySendReply(roomID, messageContent, parentEventID, post.CreateAt, senderID)
				if sendErr != nil {
					result.Stats.RepliesFailed++
					result.Errors = append(result.Errors, fmt.Sprintf("Failed to send reply %s: %v", post.ID, sendErr))
					if progress != nil {
						progress(idx+1, total, post.ChannelID, "failed:reply_error")
					}
					continue
				}
				eventID = resp.EventID
				result.Stats.RepliesImported++
			}
		} else {
			resp, sendErr := i.trySendMessage(roomID, messageContent, post.CreateAt, senderID)
			if sendErr != nil {
				result.Stats.MessagesFailed++
				result.Errors = append(result.Errors, fmt.Sprintf("Failed to send message %s: %v", post.ID, sendErr))
				if progress != nil {
					progress(idx+1, total, post.ChannelID, "failed:send_error")
				}
				continue
			}
			eventID = resp.EventID
		}
		
		// Store mapping
		result.Mapping[post.ID] = eventID
		result.Stats.MessagesImported++

		// Upload files directly from the local Mattermost data directory
		if fileConfig.Mode == "local" && len(files) > 0 {
			if fileConfig.LocalDataPath == "" {
				logger.Warn("local_data_path not configured; skipping %d file(s) for post %s", len(files), post.ID)
				result.Stats.FilesSkipped += len(files)
			} else {
				for _, file := range files {
					if err := i.uploadLocalFile(roomID, file, fileConfig.LocalDataPath, fileConfig.MaxUploadSize, post.CreateAt, senderID); err != nil {
						logger.Warn("File upload failed [%s]: %v", file.Name, err)
						result.Stats.FilesSkipped++
						result.Errors = append(result.Errors, fmt.Sprintf("upload failed for %s (post %s): %v", file.Name, post.ID, err))
					} else {
						result.Stats.FilesUploaded++
					}
				}
			}
		}

		if progress != nil {
			progress(idx+1, total, post.ChannelID, "imported")
		}

		// Log progress every 100 messages
		if (idx+1)%100 == 0 {
			logger.Info("Message import progress: %d/%d (%.1f%%) - uploaded: %d, linked: %d, skipped: %d",
				idx+1, total, float64(idx+1)/float64(total)*100,
				result.Stats.FilesUploaded, result.Stats.FilesLinked, result.Stats.FilesSkipped)
		}
	}

	logger.Info("Message import completed: imported=%d, skipped=%d, failed=%d, replies=%d, files_uploaded=%d, files_linked=%d, files_skipped=%d",
		result.Stats.MessagesImported, result.Stats.MessagesSkipped,
		result.Stats.MessagesFailed, result.Stats.RepliesImported,
		result.Stats.FilesUploaded, result.Stats.FilesLinked, result.Stats.FilesSkipped)
	
	return result, nil
}

