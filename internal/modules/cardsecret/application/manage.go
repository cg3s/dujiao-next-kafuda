package application

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/dujiao-next/internal/constants"
	cardsecretcontract "github.com/dujiao-next/internal/modules/cardsecret/contract"
	cardsecretdomain "github.com/dujiao-next/internal/modules/cardsecret/domain"
)

// ListCardSecretInput 卡密列表输入
type ListCardSecretInput struct {
	ProductID uint
	SKUID     uint
	BatchID   uint
	Status    string
	Secret    string
	BatchNo   string
	Page      int
	PageSize  int
}

// ListCardSecrets 获取卡密列表
func (s *Service) ListCardSecrets(input ListCardSecretInput) ([]cardsecretdomain.Secret, int64, error) {
	if input.SKUID > 0 && input.ProductID == 0 {
		return nil, 0, ErrInvalid
	}
	if input.ProductID > 0 && input.SKUID > 0 {
		if _, err := s.resolveCardSecretSKU(input.ProductID, input.SKUID); err != nil {
			return nil, 0, err
		}
	}

	items, total, err := s.secretRepo.List(cardsecretcontract.ListFilter{
		ProductID: input.ProductID,
		SKUID:     input.SKUID,
		BatchID:   input.BatchID,
		Status:    strings.TrimSpace(input.Status),
		Secret:    strings.TrimSpace(input.Secret),
		BatchNo:   strings.TrimSpace(input.BatchNo),
		Page:      input.Page,
		PageSize:  input.PageSize,
	})
	if err != nil {
		return nil, 0, ErrFetchFailed
	}
	return items, total, nil
}

func (s *Service) buildRepositoryFilter(input ListCardSecretInput) cardsecretcontract.ListFilter {
	return cardsecretcontract.ListFilter{
		ProductID: input.ProductID,
		SKUID:     input.SKUID,
		BatchID:   input.BatchID,
		Status:    strings.TrimSpace(input.Status),
		Secret:    strings.TrimSpace(input.Secret),
		BatchNo:   strings.TrimSpace(input.BatchNo),
		Page:      input.Page,
		PageSize:  input.PageSize,
	}
}

func (s *Service) hasListFilter(input ListCardSecretInput) bool {
	filter := s.buildRepositoryFilter(input)
	return filter.ProductID > 0 ||
		filter.SKUID > 0 ||
		filter.BatchID > 0 ||
		filter.Status != "" ||
		filter.Secret != "" ||
		filter.BatchNo != ""
}

// BatchUpdateCardSecretStatus 批量更新卡密状态
func (s *Service) BatchUpdateCardSecretStatus(ids []uint, batchID uint, filter ListCardSecretInput, status string) (int64, error) {
	normalizedStatus := strings.TrimSpace(status)
	switch normalizedStatus {
	case cardsecretdomain.StatusAvailable, cardsecretdomain.StatusReserved, cardsecretdomain.StatusUsed:
	default:
		return 0, ErrInvalid
	}
	normalizedIDs, err := s.resolveBatchTargetCardSecretIDs(ids, batchID, filter)
	if err != nil {
		return 0, err
	}
	if normalizedStatus == cardsecretdomain.StatusUsed {
		rows, err := s.secretRepo.BatchUpdateStatus(normalizedIDs, normalizedStatus, time.Now())
		if err != nil {
			return 0, ErrUpdateFailed
		}
		return rows, nil
	}

	targets, err := s.secretRepo.ListByIDs(normalizedIDs)
	if err != nil {
		return 0, ErrFetchFailed
	}
	if len(targets) == 0 {
		return 0, nil
	}
	if s.transactions == nil {
		return 0, ErrUpdateFailed
	}
	productIDs := make([]uint, 0)
	originalProducts := make(map[uint]uint, len(targets))
	stockSKUIDs := make(map[uint]map[uint]bool)
	for _, target := range targets {
		originalProducts[target.ID] = target.ProductID
		if _, seen := stockSKUIDs[target.ProductID]; !seen {
			productIDs = append(productIDs, target.ProductID)
			stockSKUIDs[target.ProductID] = nil
		}
	}

	var rows int64
	err = s.transactions.Transaction(func(secrets cardsecretcontract.Repository, _ cardsecretcontract.BatchRepository, products cardsecretcontract.ProductRepository, skus cardsecretcontract.ProductSKURepository) error {
		if products == nil || skus == nil {
			return ErrUpdateFailed
		}
		if err := products.LockByIDs(productIDs); err != nil {
			return ErrUpdateFailed
		}
		for _, productID := range productIDs {
			product, err := products.GetByID(strconv.FormatUint(uint64(productID), 10))
			if err != nil {
				return err
			}
			if product == nil || constants.IsDirectRechargeFulfillment(strings.TrimSpace(product.FulfillmentType)) {
				continue
			}
			items, err := skus.ListByProduct(productID, false)
			if err != nil {
				return err
			}
			valid := make(map[uint]bool, len(items)+1)
			valid[0] = true // Legacy cards may have no SKU association.
			for _, sku := range items {
				valid[sku.ID] = true
			}
			stockSKUIDs[productID] = valid
		}
		current, err := secrets.ListByIDs(normalizedIDs)
		if err != nil {
			return err
		}
		eligible := make([]uint, 0, len(current))
		for _, item := range current {
			if productID, found := originalProducts[item.ID]; found && productID == item.ProductID && stockSKUIDs[productID][item.SKUID] {
				eligible = append(eligible, item.ID)
			}
		}
		rows, err = secrets.BatchUpdateStatus(eligible, normalizedStatus, time.Now())
		return err
	})
	if err != nil {
		return 0, ErrUpdateFailed
	}
	return rows, nil
}

// BatchDeleteCardSecrets 批量删除卡密
func (s *Service) BatchDeleteCardSecrets(ids []uint, batchID uint, filter ListCardSecretInput) (int64, error) {
	normalizedIDs, err := s.resolveBatchTargetCardSecretIDs(ids, batchID, filter)
	if err != nil {
		return 0, err
	}
	rows, err := s.secretRepo.BatchDeleteByIDs(normalizedIDs)
	if err != nil {
		return 0, ErrDeleteFailed
	}
	return rows, nil
}

// UpdateCardSecret 更新卡密
func (s *Service) UpdateCardSecret(id uint, secret, status string) (*cardsecretdomain.Secret, error) {
	if id == 0 {
		return nil, ErrInvalid
	}
	item, err := s.secretRepo.GetByID(id)
	if err != nil {
		return nil, ErrFetchFailed
	}
	if item == nil {
		return nil, ErrNotFound
	}
	trimmedSecret := strings.TrimSpace(secret)
	trimmedStatus := strings.TrimSpace(status)
	switch trimmedStatus {
	case "", cardsecretdomain.StatusAvailable, cardsecretdomain.StatusReserved, cardsecretdomain.StatusUsed:
	default:
		return nil, ErrInvalid
	}
	if s.transactions == nil {
		return nil, ErrUpdateFailed
	}
	err = s.transactions.Transaction(func(secrets cardsecretcontract.Repository, _ cardsecretcontract.BatchRepository, products cardsecretcontract.ProductRepository, skus cardsecretcontract.ProductSKURepository) error {
		restoresStock := trimmedStatus == cardsecretdomain.StatusAvailable || trimmedStatus == cardsecretdomain.StatusReserved
		if restoresStock {
			if products == nil || skus == nil {
				return ErrUpdateFailed
			}
			if err := products.LockByIDs([]uint{item.ProductID}); err != nil {
				return ErrUpdateFailed
			}
		}
		current, err := secrets.GetByID(id)
		if err != nil {
			return ErrFetchFailed
		}
		if current == nil {
			return ErrNotFound
		}
		if restoresStock {
			if current.ProductID != item.ProductID {
				return ErrInvalid
			}
			product, err := products.GetByID(strconv.FormatUint(uint64(current.ProductID), 10))
			if err != nil {
				return ErrUpdateFailed
			}
			if product == nil || constants.IsDirectRechargeFulfillment(strings.TrimSpace(product.FulfillmentType)) {
				return ErrInvalid
			}
			if current.SKUID != 0 {
				sku, err := skus.GetByID(current.SKUID)
				if err != nil {
					return ErrUpdateFailed
				}
				if sku == nil || sku.ProductID != current.ProductID {
					return ErrInvalid
				}
			}
		}
		if err := secrets.Update(&cardsecretdomain.Secret{
			ID: id, Secret: trimmedSecret, Status: trimmedStatus, UpdatedAt: time.Now(),
		}); err != nil {
			return ErrUpdateFailed
		}
		item, err = secrets.GetByID(id)
		if err != nil {
			return ErrFetchFailed
		}
		if item == nil {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrFetchFailed) {
			return nil, err
		}
		return nil, ErrUpdateFailed
	}
	return item, nil
}
