package application_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/dujiao-next/internal/app/container"
	"github.com/dujiao-next/internal/app/httpserver/middleware"
	channelwiring "github.com/dujiao-next/internal/bootstrap/channelapi"
	"github.com/dujiao-next/internal/config"
	"github.com/dujiao-next/internal/constants"
	categoryapp "github.com/dujiao-next/internal/modules/catalog/category/application"
	categorystore "github.com/dujiao-next/internal/modules/catalog/category/infrastructure/gormstore"
	mappingstore "github.com/dujiao-next/internal/modules/catalog/mapping/infrastructure/gormstore"
	productapp "github.com/dujiao-next/internal/modules/catalog/product/application"
	productstore "github.com/dujiao-next/internal/modules/catalog/product/store/gormstore"
	channelhttp "github.com/dujiao-next/internal/modules/channelapi/transport/http"
	channelclientapp "github.com/dujiao-next/internal/modules/channelclient/application"
	channelclientdomain "github.com/dujiao-next/internal/modules/channelclient/domain"
	channelclientstore "github.com/dujiao-next/internal/modules/channelclient/infrastructure/gormstore"
	externalidentitydomain "github.com/dujiao-next/internal/modules/identity/externalidentity/domain"
	externalidentitystore "github.com/dujiao-next/internal/modules/identity/externalidentity/infrastructure/gormstore"
	userauthapp "github.com/dujiao-next/internal/modules/identity/userauth/application"
	memberapp "github.com/dujiao-next/internal/modules/memberlevel/application"
	memberstore "github.com/dujiao-next/internal/modules/memberlevel/infrastructure/gormstore"
	orderapp "github.com/dujiao-next/internal/modules/order/application"
	paymentstore "github.com/dujiao-next/internal/modules/payment/infrastructure/gormstore"
	settingsapp "github.com/dujiao-next/internal/modules/settings/application"
	settingsstore "github.com/dujiao-next/internal/modules/settings/infrastructure/gormstore"
	"github.com/dujiao-next/internal/platform/http/response"
	"github.com/dujiao-next/internal/upstream"
	"github.com/gin-gonic/gin"
)

func TestAuthenticatedChannelRejectsEligibleDirectSKU(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newDirectCheckoutFixture(t)
	orders := orderapp.NewOrderService(f.options)
	preview, err := orders.PreviewOrder(f.input)
	if err != nil || preview == nil || len(preview.Items) != 1 || preview.Items[0].SKUID != f.sku.ID {
		t.Fatalf("main storefront cannot preview the selected Direct SKU: %+v, %v", preview, err)
	}

	if err := f.db.AutoMigrate(&channelclientdomain.Client{}, &externalidentitydomain.Identity{}); err != nil {
		t.Fatal(err)
	}
	const channelUserID = "4815162342"
	identities := externalidentitystore.New(f.db)
	if err := identities.Create(&externalidentitydomain.Identity{
		UserID: f.input.UserID, Provider: constants.UserOAuthProviderTelegram, ProviderUserID: channelUserID,
	}); err != nil {
		t.Fatal(err)
	}
	const appSecret = "test-channel-checkout-app-secret"
	clientStore := &channelCheckoutClientStore{Store: channelclientstore.New(f.db), markedUsed: make(chan error, 1)}
	clients := channelclientapp.NewService(clientStore, appSecret)
	client, err := clients.CreateChannelClient("Direct checkout test", "telegram_bot", "", "", "")
	if err != nil {
		t.Fatal(err)
	}

	categories := categorystore.NewCategoryStore(f.db)
	products := productstore.NewProductStore(f.db)
	settings := settingsapp.NewService(settingsstore.New(f.db))
	c := &container.Container{
		CategoryService: categoryapp.NewService(categories), CategoryRepo: categories,
		ProductReadService: productapp.NewService(productapp.Options{Products: products, Categories: categories}), ProductRepo: products,
		ProductMappingRepo: mappingstore.NewMappingStore(f.db), SKUMappingRepo: mappingstore.NewSKUMappingStore(f.db),
		UserAuthService:    userauthapp.NewService(&config.Config{}, f.options.UserStore, identities, nil, settings, nil, nil),
		MemberLevelService: memberapp.NewService(memberstore.NewLevelStore(f.db), memberstore.NewPriceStore(f.db), memberstore.NewUserStore(f.db), memberstore.NewUserPriceStore(f.db)),
		SettingService:     settings, OrderService: orders, PaymentStore: paymentstore.New(f.db, appSecret), ChannelClientService: clients,
	}
	router := gin.New()
	channelhttp.RegisterRoutes(router.Group("/api/v1/channel", middleware.ChannelAPIAuthMiddleware(c)), channelwiring.NewHandler(c))

	// The signed client cannot opt in to the main storefront's purchase context.
	body := fmt.Appendf(nil, `{"channel_user_id":%q,"storefront_request":true,"items":[{"product_id":%d,"sku_id":%d,"quantity":1}]}`, channelUserID, f.product.ID, f.sku.ID)
	for _, path := range []string{"/api/v1/channel/orders/preview", "/api/v1/channel/orders"} {
		t.Run(path, func(t *testing.T) {
			timestamp := time.Now().Unix()
			request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Dujiao-Next-Channel-Key", client.ChannelKey)
			request.Header.Set("Dujiao-Next-Channel-Timestamp", strconv.FormatInt(timestamp, 10))
			request.Header.Set("Dujiao-Next-Channel-Signature", upstream.Sign(client.ChannelSecret, request.Method, path, timestamp, body))
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			var result response.ChannelResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || recorder.Code != http.StatusBadRequest || result.ErrorCode != "product_unavailable" || result.Data != nil {
				t.Errorf("channel accepted or improperly rejected the Direct SKU: %d %s, %v", recorder.Code, recorder.Body, err)
			}
			select {
			case err := <-clientStore.markedUsed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("signed request did not record authenticated channel usage")
			}
		})
	}
	assertNoDirectCheckoutRows(t, f)
}

type channelCheckoutClientStore struct {
	*channelclientstore.Store
	markedUsed chan error
}

func (s *channelCheckoutClientStore) UpdateLastUsed(id uint, usedAt time.Time) error {
	err := s.Store.UpdateLastUsed(id, usedAt)
	s.markedUsed <- err
	return err
}
