package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/tgdrive/teldrive/internal/auth"
	"github.com/tgdrive/teldrive/internal/cache"
	"github.com/tgdrive/teldrive/internal/config"
	"github.com/tgdrive/teldrive/internal/kv"
	"github.com/tgdrive/teldrive/internal/tgc"
	"github.com/tgdrive/teldrive/pkg/models"
	"github.com/tgdrive/teldrive/pkg/schemas"
	"github.com/tgdrive/teldrive/pkg/types"
	"golang.org/x/sync/errgroup"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type UserService struct {
	db    *gorm.DB
	cnf   *config.Config
	kv    kv.KV
	cache cache.Cacher
}

func NewUserService(db *gorm.DB, cnf *config.Config, kv kv.KV, cache cache.Cacher) *UserService {
	return &UserService{db: db, cnf: cnf, kv: kv, cache: cache}
}
func (us *UserService) GetProfilePhoto(c *gin.Context) {
	_, session := auth.GetUser(c)

	client, err := tgc.AuthClient(c, &us.cnf.TG, session)

	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	err = tgc.RunWithAuth(c, client, "", func(ctx context.Context) error {
		self, err := client.Self(c)
		if err != nil {
			return err
		}
		peer := self.AsInputPeer()
		if self.Photo == nil {
			return nil
		}
		photo, ok := self.Photo.AsNotEmpty()
		if !ok {
			return errors.New("profile not found")
		}
		location := &tg.InputPeerPhotoFileLocation{Big: false, Peer: peer, PhotoID: photo.PhotoID}
		buff, err := tgc.GetMediaContent(c, client.API(), location)
		if err != nil {
			return err
		}
		content := buff.Bytes()
		c.Writer.Header().Set("Content-Type", "image/jpeg")
		c.Writer.Header().Set("Cache-Control", "public, max-age=86400")
		c.Writer.Header().Set("Content-Length", strconv.Itoa(len(content)))
		c.Writer.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", "profile.jpeg"))
		c.Writer.Write(content)
		return nil
	})
	if err != nil {
		c.AbortWithError(http.StatusNotFound, err)
		return
	}
}

func (us *UserService) GetStats(c *gin.Context) (*schemas.AccountStats, *types.AppError) {
	userID, _ := auth.GetUser(c)
	var (
		channelId int64
		err       error
	)

	channelId, _ = getDefaultChannel(us.db, us.cache, userID)

	tokens, err := getBotsToken(us.db, us.cache, userID, channelId)

	if err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusInternalServerError}
	}
	return &schemas.AccountStats{Bots: tokens, ChannelID: channelId}, nil
}

func (us *UserService) UpdateChannel(c *gin.Context) (*schemas.Message, *types.AppError) {

	userId, _ := auth.GetUser(c)

	var payload schemas.Channel

	if err := c.ShouldBindJSON(&payload); err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusBadRequest}
	}

	channel := &models.Channel{ChannelID: payload.ChannelID, ChannelName: payload.ChannelName, UserID: userId,
		Selected: true}

	if err := us.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "channel_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{"selected": true}),
	}).Create(channel).Error; err != nil {
		return nil, &types.AppError{Error: errors.New("failed to update channel"),
			Code: http.StatusInternalServerError}
	}
	us.db.Model(&models.Channel{}).Where("channel_id != ?", payload.ChannelID).
		Where("user_id = ?", userId).Update("selected", false)

	key := fmt.Sprintf("users:channel:%d", userId)
	us.cache.Set(key, payload.ChannelID, 0)
	return &schemas.Message{Message: "channel updated"}, nil
}

func (us *UserService) ListSessions(c *gin.Context) ([]schemas.SessionOut, *types.AppError) {
	userId, userSession := auth.GetUser(c)

	client, _ := tgc.AuthClient(c, &us.cnf.TG, userSession)

	var (
		auth *tg.AccountAuthorizations
		err  error
	)

	err = client.Run(c, func(ctx context.Context) error {
		auth, err = client.API().AccountGetAuthorizations(c)
		if err != nil {
			return err
		}
		return nil
	})

	if err != nil && !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
		return nil, &types.AppError{Error: err}
	}

	dbSessions := []models.Session{}

	if err = us.db.Where("user_id = ?", userId).Order("created_at DESC").Find(&dbSessions).Error; err != nil {
		return nil, &types.AppError{Error: err}
	}

	sessionsOut := []schemas.SessionOut{}

	for _, session := range dbSessions {

		s := schemas.SessionOut{Hash: session.Hash,
			CreatedAt: session.CreatedAt.UTC().Format(time.RFC3339),
			Current:   session.Session == userSession}

		if auth != nil {
			for _, a := range auth.Authorizations {
				if session.SessionDate == a.DateCreated {
					s.AppName = strings.Trim(strings.Replace(a.AppName, "Telegram", "", -1), " ")
					s.Location = a.Country
					s.OfficialApp = a.OfficialApp
					s.Valid = true
					break
				}
			}
		}

		sessionsOut = append(sessionsOut, s)
	}

	return sessionsOut, nil
}

func (us *UserService) RemoveSession(c *gin.Context) (*schemas.Message, *types.AppError) {

	userId, _ := auth.GetUser(c)

	session := &models.Session{}

	if err := us.db.Where("user_id = ?", userId).Where("hash = ?", c.Param("id")).First(session).Error; err != nil {
		return nil, &types.AppError{Error: err}
	}

	client, _ := tgc.AuthClient(c, &us.cnf.TG, session.Session)

	client.Run(c, func(ctx context.Context) error {
		_, err := client.API().AuthLogOut(c)
		if err != nil {
			return err
		}
		return nil
	})

	us.db.Where("user_id = ?", userId).Where("hash = ?", session.Hash).Delete(&models.Session{})

	return &schemas.Message{Message: "session deleted"}, nil
}

func (us *UserService) ListChannels(c *gin.Context) ([]schemas.Channel, *types.AppError) {
	_, session := auth.GetUser(c)
	client, _ := tgc.AuthClient(c, &us.cnf.TG, session)

	channels := make(map[int64]*schemas.Channel)

	client.Run(c, func(ctx context.Context) error {

		dialogs, _ := query.GetDialogs(client.API()).BatchSize(100).Collect(ctx)

		for _, dialog := range dialogs {
			if !dialog.Deleted() {
				for _, channel := range dialog.Entities.Channels() {
					_, exists := channels[channel.ID]
					if !exists && channel.AdminRights.AddAdmins {
						channels[channel.ID] = &schemas.Channel{ChannelID: channel.ID, ChannelName: channel.Title}
					}
				}
			}
		}
		return nil

	})
	res := []schemas.Channel{}

	for _, channel := range channels {
		res = append(res, *channel)

	}

	sort.Slice(res, func(i, j int) bool {
		return res[i].ChannelName < res[j].ChannelName
	})
	return res, nil
}

func (us *UserService) AddBots(c *gin.Context) (*schemas.Message, *types.AppError) {
	userId, session := auth.GetUser(c)
	client, _ := tgc.AuthClient(c, &us.cnf.TG, session)

	var botsTokens []string

	if err := c.ShouldBindJSON(&botsTokens); err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusBadRequest}
	}

	if len(botsTokens) == 0 {
		return &schemas.Message{Message: "no bots to add"}, nil
	}

	channelId, err := getDefaultChannel(us.db, us.cache, userId)

	if err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusInternalServerError}
	}

	return us.addBots(c, client, userId, channelId, botsTokens)

}

func (us *UserService) RemoveBots(c *gin.Context) (*schemas.Message, *types.AppError) {

	userID, _ := auth.GetUser(c)

	channelId, err := getDefaultChannel(us.db, us.cache, userID)

	if err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusInternalServerError}
	}

	if err := us.db.Where("user_id = ?", userID).Where("channel_id = ?", channelId).
		Delete(&models.Bot{}).Error; err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusInternalServerError}
	}

	us.cache.Delete(fmt.Sprintf("users:bots:%d:%d", userID, channelId))

	return &schemas.Message{Message: "bots deleted"}, nil

}

func (us *UserService) addBots(c context.Context, client *telegram.Client, userId int64, channelId int64, botsTokens []string) (*schemas.Message, *types.AppError) {

	botInfoMap := make(map[string]*types.BotInfo)

	err := tgc.RunWithAuth(c, client, "", func(ctx context.Context) error {

		channel, err := tgc.GetChannelById(ctx, client.API(), channelId)

		if err != nil {
			return err
		}

		g, _ := errgroup.WithContext(ctx)

		g.SetLimit(8)

		mapMu := sync.Mutex{}

		for _, token := range botsTokens {
			g.Go(func() error {
				info, err := tgc.GetBotInfo(c, us.kv, &us.cnf.TG, token)
				if err != nil {
					return err
				}
				botPeerClass, err := peer.DefaultResolver(client.API()).ResolveDomain(ctx, info.UserName)
				if err != nil {
					return err
				}
				botPeer := botPeerClass.(*tg.InputPeerUser)
				info.AccessHash = botPeer.AccessHash
				mapMu.Lock()
				botInfoMap[token] = info
				mapMu.Unlock()
				return nil
			})

		}
		if err = g.Wait(); err != nil {
			return err
		}
		if len(botsTokens) == len(botInfoMap) {
			users := []tg.InputUser{}
			for _, info := range botInfoMap {
				users = append(users, tg.InputUser{UserID: info.Id, AccessHash: info.AccessHash})
			}
			for _, user := range users {
				payload := &tg.ChannelsEditAdminRequest{
					Channel: channel,
					UserID:  tg.InputUserClass(&user),
					AdminRights: tg.ChatAdminRights{
						ChangeInfo:     false,
						PostMessages:   true,
						EditMessages:   true,
						DeleteMessages: true,
						BanUsers:       false,
						InviteUsers:    true,
						PinMessages:    false,
						ManageCall:     false,
						Other:          true,
						ManageTopics:   false,
					},
					Rank: "bot",
				}
				_, err := client.API().ChannelsEditAdmin(ctx, payload)
				if err != nil {
					return err
				}
			}
		} else {
			return errors.New("failed to fetch bots")
		}
		return nil
	})

	if err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusInternalServerError}
	}

	payload := []models.Bot{}

	for _, info := range botInfoMap {
		payload = append(payload, models.Bot{UserID: userId, Token: info.Token, BotID: info.Id,
			BotUserName: info.UserName, ChannelID: channelId,
		})
	}

	us.cache.Delete(fmt.Sprintf("users:bots:%d:%d", userId, channelId))

	if err := us.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&payload).Error; err != nil {
		return nil, &types.AppError{Error: err, Code: http.StatusInternalServerError}
	}

	return &schemas.Message{Message: "bots added"}, nil

}
