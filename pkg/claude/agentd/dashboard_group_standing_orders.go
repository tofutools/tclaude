package agentd

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/standingorders"
)

type dashboardGroupStandingOrder struct {
	Order    standingorders.OrderView `json:"order"`
	Assigned bool                     `json:"assigned"`
	Primary  bool                     `json:"primary"`
	Global   bool                     `json:"global"`
}

type dashboardGroupStandingOrdersResponse struct {
	Group  string                        `json:"group"`
	Orders []dashboardGroupStandingOrder `json:"orders"`
}

func dashboardListGroupStandingOrders(
	w http.ResponseWriter,
	_ *http.Request,
	group *db.AgentGroup,
) {
	orders, err := db.ListStandingOrders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "list standing orders: "+err.Error())
		return
	}
	groupNames := map[int64]string{group.ID: group.Name}
	response := dashboardGroupStandingOrdersResponse{
		Group:  group.Name,
		Orders: make([]dashboardGroupStandingOrder, 0, len(orders)),
	}
	for _, order := range orders {
		response.Orders = append(response.Orders,
			dashboardGroupStandingOrderView(order, group, groupNames))
	}
	writeJSON(w, http.StatusOK, response)
}

func dashboardSetGroupStandingOrder(
	w http.ResponseWriter,
	r *http.Request,
	group *db.AgentGroup,
) {
	orderID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || orderID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"standing-order id must be a positive integer")
		return
	}
	rowVersion, err := strconv.ParseInt(r.URL.Query().Get("row_version"), 10, 64)
	if err != nil || rowVersion <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"row_version is required")
		return
	}
	order, err := db.GetStandingOrder(orderID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if order == nil {
		writeError(w, http.StatusNotFound, "not_found", "standing order not found")
		return
	}
	if order.IsGlobalTarget() {
		writeError(w, http.StatusConflict, "global_scope",
			"this order already applies globally; change its primary target in Automations")
		return
	}
	assigned := r.Method == http.MethodPost
	order, err = db.SetStandingOrderGroupScope(
		orderID, group.ID, rowVersion, assigned)
	if err != nil {
		writeDashboardStandingOrderError(w, "set group scope", err)
		return
	}
	groupNames := map[int64]string{group.ID: group.Name}
	writeJSON(w, http.StatusOK,
		dashboardGroupStandingOrderView(order, group, groupNames))
}

func dashboardGroupStandingOrderView(
	order *db.StandingOrder,
	group *db.AgentGroup,
	groupNames map[int64]string,
) dashboardGroupStandingOrder {
	primary := order.IsGroupTarget() && order.GroupID == group.ID
	return dashboardGroupStandingOrder{
		Order:    dashboardStandingOrderView(order, groupNames),
		Assigned: primary || slices.Contains(order.AdditionalGroupIDs, group.ID),
		Primary:  primary,
		Global:   order.IsGlobalTarget(),
	}
}
