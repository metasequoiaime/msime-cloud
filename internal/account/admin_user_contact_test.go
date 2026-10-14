package account

import "testing"

func TestAdminUserContactPriorityAndMasking(t *testing.T) {
	db := testStore(t)
	for _, query := range []string{
		`INSERT INTO auth_users(id) VALUES('email'),('provider'),('long-phone'),('short-phone'),('empty'),('invalid-provider')`,
		`INSERT INTO auth_identities(provider,subject,user_id,email,created_at) VALUES
 ('phone','+8613812342201','email','',now()-interval '3 days'),
 ('google','email-provider','email','provider@example.test',now()-interval '2 days'),
 ('email','first@example.test','email','',now()-interval '1 day'),
 ('email','later@example.test','email','',now()),
 ('phone','+8613812342202','provider','',now()-interval '3 days'),
 ('google','provider-first','provider','Alpha@example.test',now()-interval '2 days'),
 ('github','provider-later','provider','beta@example.test',now()-interval '1 day'),
 ('phone','+8613812342203','long-phone','',now()),
 ('phone','+123456789','short-phone','',now()),
 ('google','no-email','invalid-provider','',now()),
 ('github','bad-email','invalid-provider','invalid',now())`,
	} {
		if _, err := db.pool.Exec(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		id, contact, kind string
	}{
		{"email", "f***@example.test", "email"},
		{"provider", "A***@example.test", "email"},
		{"long-phone", "+86****2203", "phone"},
		{"short-phone", "+12****89", "phone"},
		{"empty", "", ""},
		{"invalid-provider", "", ""},
	} {
		t.Run(tc.id, func(t *testing.T) {
			var contact, kind string
			err := db.pool.QueryRow(t.Context(), `SELECT COALESCE(contact.contact,''),COALESCE(contact.contact_kind,'') FROM auth_users u `+userContactSQL+` WHERE u.id=$1`, tc.id).Scan(&contact, &kind)
			if err != nil || contact != tc.contact || kind != tc.kind {
				t.Fatalf("联系人为 %q/%q，期望 %q/%q：%v", contact, kind, tc.contact, tc.kind, err)
			}
		})
	}
}
