package model

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ImageJobAccountingGuard 永久保存参与异步图片的额度主体；关闭新建入口不能撤销此屏障。
// 原批量计费只在进程内存中保存增量，因此在线移交仅支持明确确认的单 writer 部署。
type ImageJobAccountingGuard struct {
	Kind      string `gorm:"primaryKey;size:8"`
	SubjectID int    `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt int64
}

// ImageJobAccountingTransfer 与额度增量、guard 同事务写入，解决提交回执丢失后的重复移交。
type ImageJobAccountingTransfer struct {
	ID           string `gorm:"primaryKey;size:36"`
	TokenID      int
	UserID       int
	TokenDelta   int
	UserDelta    int
	UsedDelta    int
	RequestDelta int
	CreatedAt    int64
}

var errImageJobAccountingFrozen = errors.New("图片额度移交尚未确认，请稍后重试")
var errImageJobAccountingUncertain = errors.New("此前批量额度写入结果不确定，核对账目后才能创建图片任务")
var imageJobAccountingIssues sync.Map // ImageJobAccountingGuard 仅使用 Kind 和 SubjectID 标识

type imageJobAccountingRetry struct {
	attempts int
	nextAt   time.Time
}

var imageJobAccounting = struct {
	sync.RWMutex
	ready    bool
	issueDir string
	tokens   map[int]bool
	keys     map[string]int
	users    map[int]bool
	pending  map[int]*ImageJobAccountingTransfer
	frozen   map[int]int // user ID -> 正在移交的 token ID
	retries  map[int]imageJobAccountingRetry
	changed  chan struct{}
}{tokens: map[int]bool{}, keys: map[string]int{}, users: map[int]bool{}, pending: map[int]*ImageJobAccountingTransfer{}, frozen: map[int]int{}, retries: map[int]imageJobAccountingRetry{}, changed: make(chan struct{})}

// InitImageJobAccountingBridge 必须在 worker、HTTP 和批量刷新启动前执行。
// single-writer 是部署前置条件，不是分布式锁；不得在多实例或新旧版本并行写额度时开启。
func InitImageJobAccountingBridge(ctx context.Context) error {
	imageJobAccounting.Lock()
	defer imageJobAccounting.Unlock()
	confirmed := os.Getenv("IMAGE_JOBS_SINGLE_WRITER") == "true"
	if !DB.Migrator().HasTable(&ImageJobAccountingGuard{}) {
		if confirmed {
			return errors.New("图片额度桥表缺失，请先完成数据库迁移")
		}
		return nil
	}
	var guards []ImageJobAccountingGuard
	if err := DB.WithContext(ctx).Find(&guards).Error; err != nil {
		return err
	}
	if len(guards) > 0 && !confirmed {
		return errors.New("已有图片额度屏障，启动必须确认 IMAGE_JOBS_SINGLE_WRITER=true")
	}
	if confirmed && !DB.Migrator().HasTable(&ImageJobAccountingTransfer{}) {
		return errors.New("图片额度移交表缺失，请先完成数据库迁移")
	}
	if confirmed {
		dataDir := os.Getenv("TWORK_IMAGE_TASKS_DATA_DIR")
		if !filepath.IsAbs(dataDir) {
			return errors.New("图片额度桥需要绝对路径 TWORK_IMAGE_TASKS_DATA_DIR 保存故障标记")
		}
		imageJobAccounting.issueDir = filepath.Join(dataDir, "accounting-issues")
		if err := os.MkdirAll(imageJobAccounting.issueDir, 0700); err != nil {
			return err
		}
		entries, err := os.ReadDir(imageJobAccounting.issueDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			kind, id, ok := strings.Cut(entry.Name(), "-")
			subjectID, parseErr := strconv.Atoi(id)
			if !ok || parseErr != nil || subjectID <= 0 || (kind != "tokerr" && kind != "usrerr") || entry.IsDir() {
				return errors.New("图片额度故障标记无效，需人工核对")
			}
			imageJobAccountingIssues.Store(ImageJobAccountingGuard{Kind: kind, SubjectID: subjectID}, true)
		}
	}
	for _, guard := range guards {
		switch guard.Kind {
		case "token":
			imageJobAccounting.tokens[guard.SubjectID] = true
		case "user":
			imageJobAccounting.users[guard.SubjectID] = true
		case "tokerr", "usrerr":
			imageJobAccountingIssues.Store(ImageJobAccountingGuard{Kind: guard.Kind, SubjectID: guard.SubjectID}, true)
		default:
			return errors.New("图片额度屏障类型无效")
		}
	}
	if len(imageJobAccounting.tokens) > 0 {
		ids := make([]int, 0, len(imageJobAccounting.tokens))
		for id := range imageJobAccounting.tokens {
			ids = append(ids, id)
		}
		var tokens []Token
		if err := DB.WithContext(ctx).Unscoped().Select("id", commonKeyCol).Where("id IN ?", ids).Find(&tokens).Error; err != nil {
			return err
		}
		for _, token := range tokens {
			imageJobAccounting.keys[common.GenerateHMAC(token.Key)] = token.Id
		}
	}
	imageJobAccounting.ready = confirmed
	return nil
}

func ImageJobAccountingBridgeReady() bool {
	imageJobAccounting.RLock()
	defer imageJobAccounting.RUnlock()
	return imageJobAccounting.ready
}

// 以下 mode 函数的调用方持有 RLock 至本次额度读写完成，阻止移交切开原批量操作。
func imageJobTokenAccountingMode(id int) (bool, error) {
	if imageJobAccounting.pending[id] != nil {
		return false, errImageJobAccountingFrozen
	}
	return imageJobAccounting.tokens[id], nil
}

func imageJobUserAccountingMode(id int) (bool, error) {
	if _, exists := imageJobAccounting.frozen[id]; exists {
		return false, errImageJobAccountingFrozen
	}
	return imageJobAccounting.users[id], nil
}

// 旧在途结算/退款不能把冻结当作失败丢弃，也不能重放结果不确定的写入。
// 只等待同一移交确认；等待时释放读锁，让恢复事务取得写锁。
// 返回后仍持有读锁，调用方必须保留至本次写入完成，再 RUnlock。
func lockImageJobAccountingMutation(id int, mode func(int) (bool, error)) bool {
	for {
		imageJobAccounting.RLock()
		guarded, err := mode(id)
		if err == nil {
			return guarded
		}
		changed := imageJobAccounting.changed
		imageJobAccounting.RUnlock()
		<-changed
	}
}

// PrepareImageJobAccounting 在图片 reserve 事务前调用。旧请求已经预扣的增量先移交，
// 之后尚未完成的旧请求按原差额规则直接更新 DB，既不重扣预扣额，也不改变旧价格。
func PrepareImageJobAccounting(ctx context.Context, tokenID int) (err error) {
	imageJobAccounting.Lock()
	defer imageJobAccounting.Unlock()
	defer func() {
		if err == nil || imageJobAccounting.pending[tokenID] == nil {
			return
		}
		retry := imageJobAccounting.retries[tokenID]
		if retry.attempts < 6 {
			retry.attempts++
		}
		delay := min(1<<(retry.attempts-1), 30)
		retry.nextAt = time.Now().Add(time.Duration(delay) * time.Second)
		imageJobAccounting.retries[tokenID] = retry
	}()
	if !imageJobAccounting.ready {
		if !common.RedisEnabled && !common.BatchUpdateEnabled {
			return nil
		}
		return errors.New("图片额度桥尚未就绪")
	}
	var token Token
	if err := DB.WithContext(ctx).Unscoped().First(&token, tokenID).Error; err != nil {
		return err
	}
	if _, uncertain := imageJobAccountingIssues.Load(ImageJobAccountingGuard{Kind: "tokerr", SubjectID: tokenID}); uncertain {
		return errImageJobAccountingUncertain
	}
	if _, uncertain := imageJobAccountingIssues.Load(ImageJobAccountingGuard{Kind: "usrerr", SubjectID: token.UserId}); uncertain {
		return errImageJobAccountingUncertain
	}
	if owner, exists := imageJobAccounting.frozen[token.UserId]; exists && owner != tokenID {
		return errImageJobAccountingFrozen
	}
	if imageJobAccounting.tokens[tokenID] && imageJobAccounting.users[token.UserId] {
		return nil
	}
	transfer := imageJobAccounting.pending[tokenID]
	if transfer == nil {
		transfer = &ImageJobAccountingTransfer{
			ID: uuid.NewString(), TokenID: tokenID, UserID: token.UserId,
			TokenDelta:   batchUpdateStores[BatchUpdateTypeTokenQuota][tokenID],
			UserDelta:    batchUpdateStores[BatchUpdateTypeUserQuota][token.UserId],
			UsedDelta:    batchUpdateStores[BatchUpdateTypeUsedQuota][token.UserId],
			RequestDelta: batchUpdateStores[BatchUpdateTypeRequestCount][token.UserId],
			CreatedAt:    time.Now().Unix(),
		}
		imageJobAccounting.pending[tokenID] = transfer
		imageJobAccounting.frozen[token.UserId] = tokenID
		imageJobAccounting.keys[common.GenerateHMAC(token.Key)] = tokenID
	}
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// token 行锁也用于重试：先等待前次不确定的 COMMIT 完成，再查询 receipt。
		// 顺序与创建、结算及 tbackend 同步一致，禁止改成 user -> token。
		var current Token
		if err := lockForUpdate(tx.Unscoped()).First(&current, tokenID).Error; err != nil {
			return err
		}
		if current.UserId != transfer.UserID {
			return errors.New("图片额度移交期间令牌所有者已变化")
		}
		var receipt ImageJobAccountingTransfer
		if err := tx.First(&receipt, "id = ?", transfer.ID).Error; err == nil {
			if receipt != *transfer {
				return errors.New("图片额度移交回执不一致")
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var user User
		if err := lockForUpdate(tx.Unscoped()).First(&user, transfer.UserID).Error; err != nil {
			return err
		}
		if transfer.TokenDelta != 0 {
			if err := tx.Unscoped().Model(&Token{}).Where("id = ?", tokenID).Updates(map[string]any{
				"remain_quota": gorm.Expr("remain_quota + ?", transfer.TokenDelta),
				"used_quota":   gorm.Expr("used_quota - ?", transfer.TokenDelta), "accessed_time": transfer.CreatedAt,
			}).Error; err != nil {
				return err
			}
		}
		if err := tx.Unscoped().Model(&User{}).Where("id = ?", user.Id).Updates(map[string]any{
			"quota": gorm.Expr("quota + ?", transfer.UserDelta), "used_quota": gorm.Expr("used_quota + ?", transfer.UsedDelta),
			"request_count": gorm.Expr("request_count + ?", transfer.RequestDelta),
		}).Error; err != nil {
			return err
		}
		guards := []ImageJobAccountingGuard{{Kind: "token", SubjectID: tokenID, CreatedAt: transfer.CreatedAt}, {Kind: "user", SubjectID: user.Id, CreatedAt: transfer.CreatedAt}}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&guards).Error; err != nil {
			return err
		}
		return tx.Create(transfer).Error
	})
	if err != nil {
		// 不知道 COMMIT 是否成功时保留 pending 并冻结主体；批量刷新也必须跳过它们。
		return err
	}
	delete(batchUpdateStores[BatchUpdateTypeTokenQuota], tokenID)
	for kind, captured := range map[int]int{BatchUpdateTypeUserQuota: transfer.UserDelta, BatchUpdateTypeUsedQuota: transfer.UsedDelta, BatchUpdateTypeRequestCount: transfer.RequestDelta} {
		remaining := batchUpdateStores[kind][transfer.UserID] - captured
		if remaining == 0 {
			delete(batchUpdateStores[kind], transfer.UserID)
		} else {
			batchUpdateStores[kind][transfer.UserID] = remaining
		}
	}
	imageJobAccounting.tokens[tokenID] = true
	imageJobAccounting.users[transfer.UserID] = true
	delete(imageJobAccounting.pending, tokenID)
	delete(imageJobAccounting.frozen, transfer.UserID)
	delete(imageJobAccounting.retries, tokenID)
	close(imageJobAccounting.changed)
	imageJobAccounting.changed = make(chan struct{})
	return nil
}

// ReconcileImageJobAccounting 由独立 accounting worker 定期调用，不依赖新建开关或已有 job。
// 只恢复可通过同一 receipt 判定的移交；没有幂等回执的旧 batch 故障仍需人工核账。
func ReconcileImageJobAccounting(ctx context.Context) error {
	imageJobAccounting.RLock()
	ids := make([]int, 0, len(imageJobAccounting.pending))
	for id := range imageJobAccounting.pending {
		if !time.Now().Before(imageJobAccounting.retries[id].nextAt) {
			ids = append(ids, id)
		}
	}
	imageJobAccounting.RUnlock()
	var errs []error
	for _, id := range ids {
		if err := PrepareImageJobAccounting(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// 批量语句报错不能判断其是否提交，也没有旧增量的幂等键。只阻止新图片使用这份余额，
// 不猜测重放旧金额。DB + 数据目录双写故障标记，任一成功即可跨重启保持屏障。
// 调用方持有 accounting 的读锁；错误不能阻止其他旧批量记录继续沿原路径执行。
func imageJobRecordBatchAccountingFailure(kind string, subjectID int) {
	if !imageJobAccounting.ready {
		return
	}
	identity := ImageJobAccountingGuard{Kind: kind, SubjectID: subjectID}
	imageJobAccountingIssues.Store(identity, true)
	marker := filepath.Join(imageJobAccounting.issueDir, kind+"-"+strconv.Itoa(subjectID))
	file, fileErr := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE, 0600)
	if fileErr == nil {
		fileErr = file.Sync()
		_ = file.Close()
	}
	if fileErr == nil {
		dir, err := os.Open(imageJobAccounting.issueDir)
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
		fileErr = err
	}
	identity.CreatedAt = time.Now().Unix()
	dbErr := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&identity).Error
	if fileErr != nil && dbErr != nil {
		common.SysError("图片额度故障标记无法持久化；必须停用新图片并人工核账，禁止直接重启放行: " + fileErr.Error() + "; " + dbErr.Error())
	}
}
