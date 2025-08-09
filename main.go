package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

const (
	commandCreateBid = "create-bid"
	commandBid       = "bid"
)

type bidState struct {
	guildID    string
	channelID  string
	roleID     string
	expiresAt  time.Time
	cancelFunc context.CancelFunc
}

var threadIDToBidState = make(map[string]*bidState)

func main() {
	_ = godotenv.Load()

	token := strings.TrimSpace(os.Getenv("DISCORD_TOKEN"))
	if token == "" {
		log.Fatalf("DISCORD_TOKEN is not set")
	}
	if !strings.HasPrefix(token, "Bot ") {
		token = "Bot " + token
	}

	bidTimeoutSeconds := 60
	if v := strings.TrimSpace(os.Getenv("BID_TIMEOUT_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			bidTimeoutSeconds = n
		}
	}

	session, err := discordgo.New(token)
	if err != nil {
		log.Fatalf("failed creating discord session: %v", err)
	}

	session.Identify.Intents = discordgo.IntentGuilds | discordgo.IntentGuildMembers | discordgo.IntentGuildMessages | discordgo.IntentMessageContent

	session.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		if i.ApplicationCommandData().Name == commandCreateBid {
			handleCreateBid(s, i, time.Duration(bidTimeoutSeconds)*time.Second)
			return
		}
		if i.ApplicationCommandData().Name == commandBid {
			handleBid(s, i)
			return
		}
	})

	if err := session.Open(); err != nil {
		log.Fatalf("cannot open connection: %v", err)
	}
	defer session.Close()

	if err := registerCommands(session); err != nil {
		log.Printf("failed to register commands: %v", err)
	}

	log.Printf("Bot is running. Timeout=%ds. Press Ctrl+C to exit.", bidTimeoutSeconds)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	if err := cleanupCommands(session); err != nil {
		log.Printf("failed to cleanup commands: %v", err)
	}
}

func registerCommands(s *discordgo.Session) error {
	commands := []*discordgo.ApplicationCommand{
		{
			Name:        commandCreateBid,
			Description: "Create a bid: makes a thread and a temporary role",
		},
		{
			Name:        commandBid,
			Description: "Join the current bid in this thread (grants temp role)",
		},
	}

	appID := s.State.User.ID
	for _, cmd := range commands {
		if _, err := s.ApplicationCommandCreate(appID, "", cmd); err != nil {
			return fmt.Errorf("create command %s: %w", cmd.Name, err)
		}
	}
	return nil
}

func cleanupCommands(s *discordgo.Session) error {
	appID := s.State.User.ID
	existing, err := s.ApplicationCommands(appID, "")
	if err != nil {
		return err
	}
	for _, cmd := range existing {
		if cmd.Name == commandCreateBid || cmd.Name == commandBid {
			_ = s.ApplicationCommandDelete(appID, "", cmd.ID)
		}
	}
	return nil
}

func handleCreateBid(s *discordgo.Session, i *discordgo.InteractionCreate, duration time.Duration) {
	guildID := i.GuildID
	channelID := i.ChannelID

	respond := func(content string) {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content,
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	roleName := fmt.Sprintf("bid-%s", i.Member.User.Username)
	role, err := s.GuildRoleCreate(guildID, &discordgo.RoleParams{Name: roleName})
	if err != nil {
		respond("Failed to create role. Ensure I have Manage Roles.")
		return
	}

	seedMsg, err := s.ChannelMessageSend(channelID, fmt.Sprintf("Starting a new bid. Temporary role: <@&%s>. A thread will be created.", role.ID))
	if err != nil {
		respond("Failed to send seed message.")
		_ = s.GuildRoleDelete(guildID, role.ID)
		return
	}

	thread, err := s.MessageThreadStartComplex(channelID, seedMsg.ID, &discordgo.ThreadStart{
		Name:                fmt.Sprintf("bid-%s", time.Now().Format("150405")),
		AutoArchiveDuration: 60,
		RateLimitPerUser:    0,
	})
	if err != nil {
		respond("Failed to create thread. Ensure I can Manage Threads.")
		_ = s.GuildRoleDelete(guildID, role.ID)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	state := &bidState{
		guildID:    guildID,
		channelID:  thread.ID,
		roleID:     role.ID,
		expiresAt:  time.Now().Add(duration),
		cancelFunc: cancel,
	}
	threadIDToBidState[thread.ID] = state

	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(duration):
			endBid(s, state)
		}
	}()

	respond(fmt.Sprintf("Bid created: <#%s>. Use /bid in the thread to join.", thread.ID))
}

func handleBid(s *discordgo.Session, i *discordgo.InteractionCreate) {
	threadID := i.ChannelID
	state, ok := threadIDToBidState[threadID]
	if !ok {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "This channel is not an active bid thread.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	memberID := i.Member.User.ID
	if err := s.GuildMemberRoleAdd(state.guildID, memberID, state.roleID); err != nil {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Failed to add role. Ensure I have Manage Roles.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("You have been added to the bid. <@&%s>", state.roleID),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func endBid(s *discordgo.Session, state *bidState) {
	_, _ = s.ChannelMessageSend(state.channelID, fmt.Sprintf("Bid has ended. <@&%s>", state.roleID))

	after := ""
	for {
		members, err := s.GuildMembers(state.guildID, after, 1000)
		if err != nil || len(members) == 0 {
			break
		}
		for _, m := range members {
			has := false
			for _, r := range m.Roles {
				if r == state.roleID {
					has = true
					break
				}
			}
			if has {
				_ = s.GuildMemberRoleRemove(state.guildID, m.User.ID, state.roleID)
			}
		}
		after = members[len(members)-1].User.ID
		if len(members) < 1000 {
			break
		}
	}

	_ = s.GuildRoleDelete(state.guildID, state.roleID)

	archived := true
	locked := true
	_, _ = s.ChannelEditComplex(state.channelID, &discordgo.ChannelEdit{Archived: &archived, Locked: &locked})

	delete(threadIDToBidState, state.channelID)
}
