package rehearsal_test

import (
	"bytes"
	"html/template"
	"net/http"

	"github.com/gin-gonic/gin"
)

func writeDirectRehearsalPage(c *gin.Context, token string, productID, skuID uint, planLabel, storefrontSlug string, online bool, adminToken string) {
	var page bytes.Buffer
	data := struct {
		Token          string
		ProductID      uint
		SKUID          uint
		PlanLabel      string
		StorefrontSlug string
		Online         bool
		AdminToken     string
	}{Token: token, ProductID: productID, SKUID: skuID, PlanLabel: planLabel, StorefrontSlug: storefrontSlug, Online: online, AdminToken: adminToken}
	if err := directRehearsalPage.Execute(&page, data); err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", page.Bytes())
}

var directRehearsalPage = template.Must(template.New("direct-rehearsal").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="rehearsal-token" content="{{.Token}}">
  {{if .AdminToken}}<meta name="rehearsal-admin-token" content="{{.AdminToken}}">{{end}}
  <title>Kafuda GPT 直充演练</title>
  <style>
    :root { color-scheme: light; color: #172e32; background: #eef3f1; font: 16px/1.6 system-ui, sans-serif; }
    * { box-sizing: border-box; }
    body { margin: 0; }
    main { width: min(760px, 100% - 32px); margin: 48px auto; }
    header { margin-bottom: 26px; }
    h1, h2, p, dl { margin: 0; }
    h1 { font-size: clamp(28px, 6vw, 42px); line-height: 1.2; }
    h2 { font-size: 19px; }
    header p, .hint, dt { color: #496166; }
    header p { margin-top: 10px; }
    section { border: 1px solid #cbd8d4; border-radius: 16px; background: #fff; padding: 24px; margin-top: 16px; }
    dl { display: grid; grid-template-columns: repeat(auto-fit, minmax(145px, 1fr)); gap: 12px; margin-top: 14px; }
    dt { font-size: 14px; }
    dd { margin: 0; font-weight: 650; overflow-wrap: anywhere; }
    fieldset { border: 0; margin: 16px 0 0; padding: 0; }
    legend { font-weight: 650; }
    label { display: inline-flex; align-items: center; gap: 6px; margin-right: 18px; padding-block: 5px; }
    button { background: #145b52; color: white; border: 1px solid #145b52; border-radius: 8px; font: inherit; font-weight: 650; padding: 8px 14px; cursor: pointer; min-height: 42px; }
    button.secondary { color: #145b52; background: white; }
    button:disabled { background: #eef2f1; color: #5a6b6b; border-color: #d4dfdc; cursor: default; }
    button:focus-visible, input:focus-visible { outline: 2px solid #876615; outline-offset: 3px; }
    .actions { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 16px; }
    .attach-order { align-items: center; }
    .attach-order label { margin-right: 0; }
    .attach-order input { flex: 1 1 160px; min-width: 0; border: 1px solid #cbd8d4; border-radius: 8px; padding: 8px 12px; font: inherit; }
    .hint { margin-top: 12px; font-size: 14px; }
    #error { margin-top: 12px; color: #a02322; }
    @media (max-width: 500px) { main { margin-block: 24px; } section { padding: 16px; } }
  </style>
</head>
<body data-product-id="{{.ProductID}}" data-sku-id="{{.SKUID}}"{{if .StorefrontSlug}} data-storefront-slug="{{.StorefrontSlug}}"{{end}}{{if .Online}} data-online="true"{{end}}>
  <main>
    <header>
      <h1>Kafuda GPT 直充演练</h1>
      <p>{{if .Online}}本地测试环境，使用模拟收银台和模拟履约结果。{{else}}本地测试环境，使用演练钱包和模拟履约结果。{{end}}</p>
      {{if .AdminToken}}
      <div class="actions">
        <button id="open-support-admin" class="secondary" type="button">打开客服后台</button>
      </div>
      {{end}}
    </header>

    {{if .StorefrontSlug}}
    <section aria-labelledby="storefront-heading">
      <h2 id="storefront-heading">店面演练</h2>
      <p class="hint">{{if .Online}}在店面选择演练支付渠道。模拟收银台回跳后，回到此页输入待付款订单号，再点击「确认模拟入账」。{{else}}在店面选择套餐并用演练钱包付款。完成支付后回到此页，输入订单号接续履约。{{end}}</p>
      <div class="actions">
        <button id="storefront-classic" type="button">打开 Classic 店面</button>
        <button id="storefront-vault" type="button">打开 Vault 店面</button>
      </div>
      <form id="attach-order-form" class="actions attach-order">
        <label for="attach-order-no">{{if .Online}}演练订单号{{else}}已付款订单号{{end}}</label>
        <input id="attach-order-no" type="text" autocomplete="off" spellcheck="false" required>
        <button id="attach-order" class="secondary" type="submit">读取店面订单</button>
      </form>
    </section>
    {{end}}

    <section aria-labelledby="checkout-heading">
      <h2 id="checkout-heading">选定套餐并付款</h2>
      <dl>
        <div><dt>套餐与地区</dt><dd>{{.PlanLabel}} · 菲律宾</dd></div>
        <div><dt>数量</dt><dd>1</dd></div>
        <div><dt>结算金额</dt><dd id="price">等待预览</dd></div>
        <div><dt>演练钱包</dt><dd id="balance">正在查询</dd></div>
      </dl>
      <div class="actions">
        <button id="preview" type="button">预览套餐</button>
        <button id="pay" type="button" disabled>用演练钱包支付</button>
      </div>
    </section>

    <section aria-labelledby="fulfillment-heading">
      <h2 id="fulfillment-heading">演练履约</h2>
      <dl aria-live="polite">
        <div><dt>订单号</dt><dd id="order-no">待创建</dd></div>
        <div><dt>直充进度</dt><dd id="stage">待付款</dd></div>
        <div><dt>状态更新时间</dt><dd id="stage-time">—</dd></div>
      </dl>
      <fieldset id="result-options" disabled>
        <legend>模拟结果</legend>
        <label><input id="result-success" type="radio" name="result" value="success" checked>成功开通</label>
        <label><input id="result-review" type="radio" name="result" value="manual_review">转人工核对</label>
        {{if .AdminToken}}<label><input id="result-manual" type="radio" name="result" value="manual_delivery">上游确认未开通</label>{{end}}
      </fieldset>
      <div class="actions">
        {{if .Online}}<button id="confirm-online-payment" type="button" disabled>确认模拟入账</button>{{end}}
        <button id="authorize" type="button" disabled>授权模拟履约</button>
        <button id="advance" type="button" disabled>推进模拟查单</button>
        <button id="refresh" class="secondary" type="button" disabled>刷新订单进度</button>
        <button id="new-order" class="secondary" type="button" disabled>开始新订单</button>
      </div>
      <p id="error" role="alert" hidden></p>
      <p class="hint">完成或进入人工核对后，可开始新订单以体验另一种结果。演练钱包额度用完后，请重启演练服务。</p>
    </section>
  </main>

  <script>
    'use strict';
    const element = id => document.getElementById(id);
    const token = document.querySelector('meta[name="rehearsal-token"]').content;
    const adminToken = document.querySelector('meta[name="rehearsal-admin-token"]')?.content || '';
    const storefrontSlug = document.body.dataset.storefrontSlug;
    const online = document.body.dataset.online === 'true';
    const items = [{
      product_id: Number(document.body.dataset.productId),
      sku_id: Number(document.body.dataset.skuId),
      quantity: 1,
    }];
    const stageLabels = Object.freeze({
      pending_payment: '待确认入账',
      waiting_session: '待买家确认',
      checking: '检查中',
      submitting: '提交中',
      processing: '充值中',
      manual_review: '人工核对中',
      manual_delivery: '待人工交付',
      completed: '已完成',
      refunded: '已退款',
    });
    const finishedStages = new Set(['manual_review', 'manual_delivery', 'completed', 'refunded']);
    let orderNo = '';
    let stage = '';
    let previewed = false;
    let paid = false;
    let busy = false;

    async function request(path, method = 'GET', data) {
      const headers = { Authorization: 'Bearer ' + token };
      if (data !== undefined) headers['Content-Type'] = 'application/json';
      const response = await fetch('/api/v1' + path, {
        method, headers, body: data === undefined ? undefined : JSON.stringify(data),
      });
      const reply = await response.json();
      if (!response.ok || reply.status_code !== 0) {
        throw new Error(reply.msg || '操作失败，请稍后重试。');
      }
      return reply.data;
    }

    function updateControls() {
      const waiting = paid && stage === 'waiting_session';
      element('preview').disabled = busy || !!orderNo;
      element('pay').disabled = busy || !previewed || !!orderNo;
      element('result-options').disabled = busy || !waiting;
      element('authorize').disabled = busy || !waiting;
      element('advance').disabled = busy || !paid || stage !== 'processing';
      element('refresh').disabled = busy || !orderNo;
      element('new-order').disabled = busy || !finishedStages.has(stage);
      if (online) element('confirm-online-payment').disabled = busy || !orderNo || paid || stage !== 'pending_payment';
      if (adminToken) element('open-support-admin').disabled = busy;
      if (storefrontSlug) {
        element('storefront-classic').disabled = busy;
        element('storefront-vault').disabled = busy;
        element('attach-order-no').disabled = busy;
        element('attach-order').disabled = busy;
      }
    }

    async function refreshBalance() {
      try {
        const balance = await request('/wallet');
        element('balance').textContent = balance.balance + ' ' + (balance.currency || 'CNY');
      } catch {
        element('balance').textContent = '暂时无法查询';
      }
    }

    function renderStage(status) {
      stage = status.stage;
      element('stage').textContent = stageLabels[stage] || '状态待确认';
      const time = new Date(status.updated_at);
      element('stage-time').textContent = Number.isNaN(time.getTime()) ? '—' : time.toLocaleString('zh-CN');
    }

    async function refreshStage(rehearsalStage = stage) {
      const status = await request('/orders/' + encodeURIComponent(orderNo) + '/direct-status');
      if (rehearsalStage === 'manual_delivery' && status.stage === 'manual_review') {
        status.stage = rehearsalStage;
      }
      renderStage(status);
    }

    async function attachOrder(candidate) {
      const path = '/orders/' + encodeURIComponent(candidate);
      const detail = await request(path);
      const isPaid = Boolean(detail.paid_at) && ['paid', 'fulfilling', 'completed'].includes(detail.status);
      const childItems = detail.children?.[0]?.items;
      const isPending = online && !detail.paid_at && detail.status === 'pending_payment' &&
        detail.children?.length === 1 && childItems?.length === 1 && childItems[0].fulfillment_type === 'gpt_direct';
      if (detail.order_no !== candidate || (!isPaid && !isPending)) {
        throw new Error(online ? '请选择待付款或已付款的直充演练订单。' : '请选择已付款的直充演练订单。');
      }
      const status = isPaid
        ? await request(path + '/direct-status')
        : { stage: detail.status, updated_at: detail.updated_at };
      orderNo = candidate;
      paid = isPaid;
      previewed = false;
      element('order-no').textContent = orderNo;
      element('price').textContent = detail.total_amount + ' ' + detail.currency;
      renderStage(status);
      void refreshBalance();
    }

    function onAction(id, action, eventName = 'click') {
      element(id).addEventListener(eventName, async event => {
        event.preventDefault();
        if (busy) return;
        busy = true;
        element('error').hidden = true;
        updateControls();
        try {
          await action();
        } catch (error) {
          element('error').textContent = error.message || '操作失败，请稍后重试。';
          element('error').hidden = false;
        } finally {
          busy = false;
          updateControls();
        }
      });
    }

    if (storefrontSlug) {
      for (const name of ['classic', 'vault']) {
        onAction('storefront-' + name, () => {
          localStorage.removeItem('user_profile');
          localStorage.setItem('user_token', token);
          window.location.assign('/products/' + encodeURIComponent(storefrontSlug) + '?template=' + name);
        });
      }
      onAction('attach-order-form', async () => {
        const candidate = element('attach-order-no').value.trim();
        if (!candidate) throw new Error('请输入演练订单号。');
        await attachOrder(candidate);
      }, 'submit');
    }

    if (adminToken) {
      onAction('open-support-admin', () => {
        localStorage.setItem('admin_token', adminToken);
        window.location.assign('/rehearsal-admin/orders');
      });
    }

    if (online) {
      onAction('confirm-online-payment', async () => {
        await request('/rehearsal/orders/' + encodeURIComponent(orderNo) + '/confirm-payment', 'POST');
        await attachOrder(orderNo);
        if (!paid) throw new Error('模拟入账尚未确认。');
      });
    }
    onAction('preview', async () => {
      previewed = false;
      const preview = await request('/orders/preview', 'POST', { items });
      element('price').textContent = preview.total_amount + ' ' + preview.currency;
      previewed = true;
    });
    onAction('pay', async () => {
      const order = await request('/orders/create-and-pay', 'POST', { items, use_balance: true });
      orderNo = order.order_no;
      element('order-no').textContent = orderNo;
      paid = order.order_paid === true;
      void refreshBalance();
      if (!paid) throw new Error('演练钱包付款尚未确认。');
      await refreshStage();
    });
    onAction('authorize', async () => {
      const result = document.querySelector('input[name="result"]:checked').value;
      await request('/rehearsal/orders/' + encodeURIComponent(orderNo) + '/authorize', 'POST', { result });
      await refreshStage();
    });
    onAction('advance', async () => {
      const advanced = await request('/rehearsal/orders/' + encodeURIComponent(orderNo) + '/advance', 'POST');
      await refreshStage(advanced.stage);
    });
    onAction('refresh', () => online && !paid ? attachOrder(orderNo) : refreshStage());
    onAction('new-order', () => {
      orderNo = '';
      stage = '';
      previewed = false;
      paid = false;
      element('price').textContent = '等待预览';
      element('order-no').textContent = '待创建';
      element('stage').textContent = '待付款';
      element('stage-time').textContent = '—';
      element('result-success').checked = true;
      if (storefrontSlug) element('attach-order-no').value = '';
      return refreshBalance();
    });
    void refreshBalance();
  </script>
</body>
</html>`))
