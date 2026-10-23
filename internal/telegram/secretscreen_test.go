package telegram

import "testing"

func TestAReadOnlyAdminStillSeesTheReadings(t *testing.T) {
	c := Config{AdminID: "1", Admins: []Admin{{ID: "3", ReadOnly: true}}}
	for _, screen := range navScreens {
		if r := route(c, tgUser{ID: 3}, "nav:"+screen); r.alert {
			t.Errorf("nav:%s was refused to a read-only admin", screen)
		}
	}
}
