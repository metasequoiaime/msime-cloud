package account

import "github.com/jackc/pgx/v5"

// queueUserProfileReads 按用户、身份列表的顺序登记资料读取，不发送或关闭批次。
// 无身份时仍返回空数组；错误映射和事务边界由调用方处理。
func queueUserProfileReads(batch *pgx.Batch, uid string, user *User, identities *[]Identity) {
	batch.Queue(userQuery, uid).QueryRow(func(row pgx.Row) error {
		return scanUser(row, user)
	})
	batch.Queue("SELECT provider,subject FROM auth_identities WHERE user_id=$1 ORDER BY provider,subject", uid).Query(func(rows pgx.Rows) error {
		var err error
		*identities, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Identity, error) {
			var identity Identity
			return identity, row.Scan(&identity.Provider, &identity.Subject)
		})
		if *identities == nil {
			*identities = []Identity{}
		}
		return err
	})
}
