package application

import (
	"strconv"

	"github.com/dujiao-next/internal/constants"
	mappingcontract "github.com/dujiao-next/internal/modules/catalog/mapping/contract"
	mappingdomain "github.com/dujiao-next/internal/modules/catalog/mapping/domain"
	productdomain "github.com/dujiao-next/internal/modules/catalog/product/domain"
)

// 文件组织约定:
//   service.go       — 端口/错误/装配 + 基础查询与 CRUD
//   import.go        — 单品导入 + 上游元数据列表 + 图片下载
//   batch_import.go  — 批量导入与上游分类自动创建
//   sync.go          — 同步流程（单品 / 全量库存 / 下单前兜底）
//   markup.go        — 加价重算
//   pricing.go       — 汇率与加价换算

type Options struct {
	Mappings     mappingcontract.MappingRepository
	SKUMappings  mappingcontract.SKUMappingRepository
	Products     mappingcontract.ProductRepository
	SKUs         mappingcontract.SKURepository
	Categories   mappingcontract.CategoryRepository
	Connections  mappingcontract.ConnectionProvider
	Media        mappingcontract.MediaRecorder
	Transactions mappingcontract.UnitOfWork
}

// Service 承载本地商品与上游站点之间的映射用例。
type Service struct {
	mappings        mappingcontract.MappingRepository
	skuMappings     mappingcontract.SKUMappingRepository
	products        mappingcontract.ProductRepository
	skus            mappingcontract.SKURepository
	categories      mappingcontract.CategoryRepository
	connections     mappingcontract.ConnectionProvider
	media           mappingcontract.MediaRecorder
	transactions    mappingcontract.UnitOfWork
	categoryCreator mappingcontract.CategoryCreator
	settings        mappingcontract.SettingsProvider
}

func NewService(options Options) (*Service, error) {
	if options.Media == nil {
		return nil, mappingcontract.ErrMediaRecorderRequired
	}
	return &Service{
		mappings:     options.Mappings,
		skuMappings:  options.SKUMappings,
		products:     options.Products,
		skus:         options.SKUs,
		categories:   options.Categories,
		connections:  options.Connections,
		media:        options.Media,
		transactions: options.Transactions,
	}, nil
}

// withMappingWrite runs local writes after upstream I/O has finished. The
// shared product lock serializes them with detachment and fulfillment changes.
func (s *Service) withMappingWrite(snapshot *mappingdomain.Mapping, apply func(*Service, *mappingdomain.Mapping) error) error {
	return s.withCurrentMapping(snapshot, func(repos mappingcontract.ImportRepositories, current *mappingdomain.Mapping, product *productdomain.Product) error {
		if !product.IsMapped || product.FulfillmentType != constants.FulfillmentTypeUpstream {
			return mappingcontract.ErrMappingNotFound
		}
		bound := *s
		bound.products, bound.skus, bound.mappings, bound.skuMappings = repos.Products, repos.SKUs, repos.Mappings, repos.SKUMappings
		return apply(&bound, current)
	})
}

func (s *Service) withCurrentMapping(snapshot *mappingdomain.Mapping, apply func(mappingcontract.ImportRepositories, *mappingdomain.Mapping, *productdomain.Product) error) error {
	if s.transactions == nil || snapshot == nil {
		return mappingcontract.ErrMappingNotFound
	}
	return s.transactions.WithinTransaction(func(repos mappingcontract.ImportRepositories) error {
		if err := repos.Products.LockByIDs([]uint{snapshot.LocalProductID}); err != nil {
			return err
		}
		current, err := repos.Mappings.GetByID(snapshot.ID)
		if err != nil {
			return err
		}
		if current == nil || current.LocalProductID != snapshot.LocalProductID || current.ConnectionID != snapshot.ConnectionID || current.UpstreamProductID != snapshot.UpstreamProductID {
			return mappingcontract.ErrMappingNotFound
		}
		product, err := repos.Products.GetByID(strconv.FormatUint(uint64(current.LocalProductID), 10))
		if err != nil {
			return err
		}
		if product == nil {
			return mappingcontract.ErrMappingNotFound
		}
		return apply(repos, current, product)
	})
}

// SetCategoryCreator 注入分类创建端口（装配时调用）。
func (s *Service) SetCategoryCreator(creator mappingcontract.CategoryCreator) {
	s.categoryCreator = creator
}

// SetSettings 注入设置端口（用于读取上游同步动态配置）。
func (s *Service) SetSettings(settings mappingcontract.SettingsProvider) {
	s.settings = settings
}

// GetByID 获取映射详情
func (s *Service) GetByID(id uint) (*mappingdomain.Mapping, error) {
	return s.mappings.GetByID(id)
}

// List 列表查询映射
func (s *Service) List(filter mappingcontract.ListFilter) ([]mappingdomain.Mapping, int64, error) {
	return s.mappings.List(filter)
}

// SetActive 启用/禁用映射
func (s *Service) SetActive(id uint, active bool) error {
	mapping, err := s.mappings.GetByID(id)
	if err != nil {
		return err
	}
	return s.withCurrentMapping(mapping, func(repos mappingcontract.ImportRepositories, current *mappingdomain.Mapping, product *productdomain.Product) error {
		if constants.IsDirectRechargeFulfillment(product.FulfillmentType) {
			return mappingcontract.ErrMappingNotFound
		}
		return repos.Mappings.UpdateActive(current.ID, active)
	})
}

// Delete 删除映射（不删除本地商品）
func (s *Service) Delete(id uint) error {
	mapping, err := s.mappings.GetByID(id)
	if err != nil {
		return err
	}
	if mapping == nil {
		return mappingcontract.ErrMappingNotFound
	}

	return s.transactions.WithinTransaction(func(repos mappingcontract.ImportRepositories) error {
		if err := repos.Products.LockByIDs([]uint{mapping.LocalProductID}); err != nil {
			return err
		}
		current, err := repos.Mappings.GetByID(id)
		if err != nil {
			return err
		}
		if current == nil || current.LocalProductID != mapping.LocalProductID {
			return mappingcontract.ErrMappingNotFound
		}
		if err := repos.SKUMappings.DeleteByProductMapping(id); err != nil {
			return err
		}
		if current.LocalProductID > 0 {
			productID := strconv.FormatUint(uint64(current.LocalProductID), 10)
			localProduct, err := repos.Products.GetByID(productID)
			if err != nil {
				return err
			}
			if localProduct != nil {
				fields := map[string]interface{}{}
				if localProduct.IsMapped {
					fields["is_mapped"] = false
				}
				if localProduct.FulfillmentType == constants.FulfillmentTypeUpstream {
					fields["fulfillment_type"] = constants.FulfillmentTypeManual
					fields["is_active"] = false
				}
				if len(fields) > 0 {
					if err := repos.Products.QuickUpdate(productID, fields); err != nil {
						return err
					}
				}
			}
		}
		return repos.Mappings.Delete(id)
	})
}

// GetSKUMappings 获取映射的 SKU 映射列表
func (s *Service) GetSKUMappings(mappingID uint) ([]mappingdomain.SKUMapping, error) {
	return s.skuMappings.ListByProductMapping(mappingID)
}

// GetMappedUpstreamIDs 获取指定连接下所有已映射的上游商品 ID
func (s *Service) GetMappedUpstreamIDs(connectionID uint) ([]uint, error) {
	return s.mappings.ListUpstreamIDsByConnection(connectionID)
}
