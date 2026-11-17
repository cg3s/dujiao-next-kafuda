package carthttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dujiao-next/internal/constants"
	"github.com/dujiao-next/internal/i18n"
	cartapp "github.com/dujiao-next/internal/modules/cart/application"
	cartcontract "github.com/dujiao-next/internal/modules/cart/contract"
	cartdomain "github.com/dujiao-next/internal/modules/cart/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
	"github.com/dujiao-next/internal/platform/http/response"

	"github.com/gin-gonic/gin"
)

type directCartHTTPStore struct{ writes int }

func (*directCartHTTPStore) ListByUser(uint) ([]cartcontract.StoredItem, error) { return nil, nil }
func (s *directCartHTTPStore) Upsert(*cartdomain.Item) error                    { s.writes++; return nil }
func (*directCartHTTPStore) DeleteByUserProductSKU(uint, uint, uint) error      { return nil }

type directCartHTTPProducts struct{}

func (directCartHTTPProducts) GetByID(string) (*productdomain.Product, error) {
	return &productdomain.Product{ID: 7, IsActive: true, FulfillmentType: constants.FulfillmentTypeKafudaGPT}, nil
}

func TestUpsertCartItemRejectsDirectRegardlessOfClientFulfillment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, locale := range []string{"zh-CN", "zh-TW", "en-US"} {
		for _, kind := range []string{constants.FulfillmentTypeManual, constants.FulfillmentTypeAuto, constants.FulfillmentTypeKafudaGPT} {
			for _, sku := range []uint{0, 19} {
				t.Run(fmt.Sprintf("%s/%s/%d", locale, kind, sku), func(t *testing.T) {
					store := &directCartHTTPStore{}
					h := NewUserHandler(cartapp.NewService(store, directCartHTTPProducts{}, nil, nil, nil))
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Set("user_id", uint(3))
					ctx.Request = httptest.NewRequest(http.MethodPost, "/cart/items", strings.NewReader(fmt.Sprintf(`{"product_id":7,"sku_id":%d,"quantity":1,"fulfillment_type":%q}`, sku, kind)))
					ctx.Request.Header.Set("Content-Type", "application/json")
					ctx.Request.Header.Set("X-Lang", locale)
					h.UpsertCartItem(ctx)
					var body response.Response
					if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body.StatusCode != response.CodeBadRequest || body.Msg != i18n.T(locale, "error.fulfillment_invalid") || store.writes != 0 {
						t.Fatalf("Direct cart request accepted or incorrect response: %s", recorder.Body.String())
					}
				})
			}
		}
	}
}

func TestRespondCartItemUpdateError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{
			name: "invalid fulfillment",
			err:  cartcontract.ErrFulfillmentInvalid,
			code: response.CodeBadRequest,
			msg:  "交付信息不合法",
		},
		{
			name: "manual stock insufficient",
			err:  cartcontract.ErrManualStockInsufficient,
			code: response.CodeBadRequest,
			msg:  "人工库存不足",
		},
		{
			name: "unknown error",
			err:  errors.New("boom"),
			code: response.CodeInternal,
			msg:  "更新订单失败",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/cart/items", nil)

			respondCartItemUpdateError(c, tt.err)

			if recorder.Code != http.StatusOK {
				t.Fatalf("HTTP status = %d, want %d", recorder.Code, http.StatusOK)
			}
			var body response.Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.StatusCode != tt.code {
				t.Fatalf("status_code = %d, want %d; body=%s", body.StatusCode, tt.code, recorder.Body.String())
			}
			if body.Msg != tt.msg {
				t.Fatalf("msg = %q, want %q; body=%s", body.Msg, tt.msg, recorder.Body.String())
			}
		})
	}
}
