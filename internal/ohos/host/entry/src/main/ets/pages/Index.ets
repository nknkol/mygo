import host, { HostMenuItem, HostMessageBox } from 'libmygo_host.so';
import { ComponentContent, LengthMetrics, NodeContent, UIContext, display } from '@kit.ArkUI';
import { webview } from '@kit.ArkWeb';
import {
  WEB_OP_BACK,
  WEB_OP_CLEAR_CACHE,
  WEB_OP_EVAL,
  WEB_OP_FORWARD,
  WEB_OP_LOAD_HTML,
  WEB_OP_LOAD_URL,
  WEB_OP_RELOAD,
  WEB_OP_SET_UA,
  WEB_OP_STOP,
  hostBridge
} from '../bridge/HostBridge';

const MENU_KIND_SEPARATOR: number = 1;
const MENU_KIND_SUBMENU: number = 2;
const MENU_KIND_CHECKBOX: number = 3;
const MENU_KIND_RADIO: number = 4;

const WEB_EV_PAGE_BEGIN: number = 1;
const WEB_EV_PAGE_END: number = 2;
const WEB_EV_TITLE: number = 3;
const WEB_EV_ERROR: number = 4;

class QueuedWebCmd {
  op: number = 0;
  arg1: string = '';
  arg2: string = '';
}

class MenuEntryModel {
  key: string = '';
  itemId: number = 0;
  label: string = '';
  labelInfo: string = '';
  enabled: boolean = true;
  checkable: boolean = false;
  isRadio: boolean = false;
  checked: boolean = false;
  hasSubmenu: boolean = false;
  subLevel: MenuLevelModel | undefined = undefined;
}

class MenuSectionModel {
  key: string = '';
  entries: MenuEntryModel[] = [];
}

class MenuLevelModel {
  sections: MenuSectionModel[] = [];
  hasCheckable: boolean = false;
  widthVp: number = 148;
}

class PopupMenuContext {
  root: MenuLevelModel = new MenuLevelModel();
  darkMode: boolean = false;
  direction: Direction = Direction.Auto;
  onSelect: (chosenId: number) => void = (_chosenId: number): void => {};
}

class MessageBoxButtonModel {
  key: string = '';
  index: number = 0;
  label: string = '';
  isDefault: boolean = false;
  isDestructive: boolean = false;
}

class MessageBoxContext {
  title: string = '';
  message: string = '';
  detail: string = '';
  checkboxLabel: string = '';
  checkboxChecked: boolean = false;
  darkMode: boolean = false;
  buttons: MessageBoxButtonModel[] = [];
  onToggleCheckbox: (checked: boolean) => void = (_checked: boolean): void => {};
  onChoose: (index: number) => void = (_index: number): void => {};
}

function measureMenuLevelWidthVp(uiCtx: UIContext, sections: MenuSectionModel[], hasCheckable: boolean): number {
  let maxRowVp: number = 128;
  for (let s: number = 0; s < sections.length; s++) {
    const entries: MenuEntryModel[] = sections[s].entries;
    for (let i: number = 0; i < entries.length; i++) {
      const ent: MenuEntryModel = entries[i];
      let labelVp: number = ent.label.length * 13;
      let infoVp: number = ent.labelInfo.length * 8;
      try {
        const labelPx: number = uiCtx.getMeasureUtils().measureText({
          textContent: ent.label,
          fontSize: 13,
          fontWeight: FontWeight.Normal
        });
        labelVp = uiCtx.px2vp(labelPx);
        if (ent.labelInfo.length > 0) {
          const infoPx: number = uiCtx.getMeasureUtils().measureText({
            textContent: ent.labelInfo,
            fontSize: 12,
            fontWeight: FontWeight.Normal
          });
          infoVp = uiCtx.px2vp(infoPx);
        }
      } catch (_) {}
      let rowVp: number = 28 + labelVp;
      if (hasCheckable) {
        rowVp += 18;
      }
      if (ent.labelInfo.length > 0) {
        rowVp += 18 + infoVp;
      }
      if (ent.hasSubmenu) {
        rowVp += 20;
      }
      if (rowVp > maxRowVp) {
        maxRowVp = rowVp;
      }
    }
  }
  return Math.min(300, Math.max(128, Math.ceil(maxRowVp + 8)));
}

function buildMenuLevel(uiCtx: UIContext, items: HostMenuItem[], prefix: string): MenuLevelModel {
  const sections: MenuSectionModel[] = [];
  let current: MenuEntryModel[] = [];
  let secIdx: number = 0;
  let hasCheckable: boolean = false;
  for (let i: number = 0; i < items.length; i++) {
    const raw: HostMenuItem = items[i];
    if (raw.kind === MENU_KIND_SEPARATOR) {
      if (current.length > 0) {
        const sec: MenuSectionModel = new MenuSectionModel();
        sec.key = prefix + '_sec_' + secIdx.toString();
        sec.entries = current;
        sections.push(sec);
        secIdx++;
        current = [];
      }
      continue;
    }
    const ent: MenuEntryModel = new MenuEntryModel();
    ent.key = prefix + '_it_' + i.toString() + '_' + raw.id.toString();
    ent.itemId = raw.id;
    ent.label = raw.label;
    ent.labelInfo = raw.accel;
    ent.enabled = raw.enabled;
    ent.checkable = raw.kind === MENU_KIND_CHECKBOX || raw.kind === MENU_KIND_RADIO;
    ent.isRadio = raw.kind === MENU_KIND_RADIO;
    ent.checked = raw.checked && ent.checkable;
    if (ent.checkable) {
      hasCheckable = true;
    }
    if (raw.kind === MENU_KIND_SUBMENU && raw.items.length > 0) {
      const sub: MenuLevelModel = buildMenuLevel(uiCtx, raw.items, ent.key);
      if (sub.sections.length > 0) {
        ent.subLevel = sub;
        ent.hasSubmenu = true;
      }
    }
    current.push(ent);
  }
  if (current.length > 0) {
    const lastSec: MenuSectionModel = new MenuSectionModel();
    lastSec.key = prefix + '_sec_' + secIdx.toString();
    lastSec.entries = current;
    sections.push(lastSec);
  }
  const level: MenuLevelModel = new MenuLevelModel();
  level.sections = sections;
  level.hasCheckable = hasCheckable;
  level.widthVp = measureMenuLevelWidthVp(uiCtx, sections, hasCheckable);
  return level;
}

@Builder
function buildMenuItemRow(ent: MenuEntryModel, hasCheckable: boolean, darkMode: boolean): void {
  Row() {
    if (hasCheckable) {
      Row() {
        Text(ent.checked ? (ent.isRadio ? '•' : '✓') : '')
          .fontSize(ent.isRadio ? 15 : 13)
          .fontWeight(FontWeight.Medium)
          .fontColor(
            ent.enabled
              ? (darkMode ? '#F1F3F5' : '#182431')
              : (darkMode ? '#66FFFFFF' : '#66182431')
          )
      }
      .width(18)
      .height('100%')
      .alignItems(VerticalAlign.Center)
      .justifyContent(FlexAlign.Start)
    }
    Text(ent.label)
      .fontSize(13)
      .fontWeight(FontWeight.Normal)
      .fontColor(
        ent.enabled
          ? (darkMode ? '#F1F3F5' : '#182431')
          : (darkMode ? '#66FFFFFF' : '#66182431')
      )
      .maxLines(1)
      .textOverflow({ overflow: TextOverflow.Ellipsis })
      .layoutWeight(1)
    if (ent.labelInfo.length > 0) {
      Text(ent.labelInfo)
        .fontSize(12)
        .fontWeight(FontWeight.Normal)
        .fontColor(darkMode ? '#8CFFFFFF' : '#8C182431')
        .margin({ left: 16 })
    }
    if (ent.hasSubmenu) {
      Text('›')
        .fontSize(16)
        .fontWeight(FontWeight.Normal)
        .fontColor(
          ent.enabled
            ? (darkMode ? '#A6FFFFFF' : '#99182431')
            : (darkMode ? '#4DFFFFFF' : '#4D182431')
        )
        .margin({ left: 12 })
    }
  }
  .width('100%')
  .height(30)
  .padding({ left: 10, right: 10 })
  .alignItems(VerticalAlign.Center)
}

@Builder
function buildLevel3SubMenu(level: MenuLevelModel, ctx: PopupMenuContext): void {
  Menu() {
    ForEach(level.sections, (sec3: MenuSectionModel) => {
      MenuItemGroup() {
        ForEach(sec3.entries, (ent3: MenuEntryModel) => {
          MenuItem((): void => {
            buildMenuItemRow(ent3, level.hasCheckable, ctx.darkMode);
          })
          .enabled(ent3.enabled)
          .onClick((): void => {
            if (ent3.enabled && ent3.itemId !== 0) {
              ctx.onSelect(ent3.itemId);
            }
          })
        }, (ent3: MenuEntryModel): string => ent3.key)
      }
    }, (sec3: MenuSectionModel): string => sec3.key)
  }
  .width(level.widthVp)
  .radius(8)
  .padding(4)
  .menuItemGroupDivider({
    strokeWidth: LengthMetrics.px(1),
    color: ctx.darkMode ? '#24FFFFFF' : '#1A000000',
    startMargin: LengthMetrics.vp(8),
    endMargin: LengthMetrics.vp(8),
    mode: DividerMode.EMBEDDED_IN_MENU
  })
  .direction(ctx.direction)
}

@Builder
function buildLevel2SubMenu(level: MenuLevelModel, ctx: PopupMenuContext): void {
  Menu() {
    ForEach(level.sections, (sec2: MenuSectionModel) => {
      MenuItemGroup() {
        ForEach(sec2.entries, (ent2: MenuEntryModel) => {
          if (ent2.hasSubmenu && ent2.subLevel !== undefined) {
            MenuItem((): void => {
              buildMenuItemRow(ent2, level.hasCheckable, ctx.darkMode);
            })
            .enabled(ent2.enabled)
            .subMenuBuilder((): void => {
              if (ent2.subLevel !== undefined) {
                buildLevel3SubMenu(ent2.subLevel, ctx);
              }
            })
          } else {
            MenuItem((): void => {
              buildMenuItemRow(ent2, level.hasCheckable, ctx.darkMode);
            })
            .enabled(ent2.enabled)
            .onClick((): void => {
              if (ent2.enabled && ent2.itemId !== 0) {
                ctx.onSelect(ent2.itemId);
              }
            })
          }
        }, (ent2: MenuEntryModel): string => ent2.key)
      }
    }, (sec2: MenuSectionModel): string => sec2.key)
  }
  .width(level.widthVp)
  .radius(8)
  .padding(4)
  .menuItemGroupDivider({
    strokeWidth: LengthMetrics.px(1),
    color: ctx.darkMode ? '#24FFFFFF' : '#1A000000',
    startMargin: LengthMetrics.vp(8),
    endMargin: LengthMetrics.vp(8),
    mode: DividerMode.EMBEDDED_IN_MENU
  })
  .direction(ctx.direction)
}

@Builder
function buildPopupMenuRoot(ctx: PopupMenuContext): void {
  Menu() {
    ForEach(ctx.root.sections, (sec1: MenuSectionModel) => {
      MenuItemGroup() {
        ForEach(sec1.entries, (ent1: MenuEntryModel) => {
          if (ent1.hasSubmenu && ent1.subLevel !== undefined) {
            MenuItem((): void => {
              buildMenuItemRow(ent1, ctx.root.hasCheckable, ctx.darkMode);
            })
            .enabled(ent1.enabled)
            .subMenuBuilder((): void => {
              if (ent1.subLevel !== undefined) {
                buildLevel2SubMenu(ent1.subLevel, ctx);
              }
            })
          } else {
            MenuItem((): void => {
              buildMenuItemRow(ent1, ctx.root.hasCheckable, ctx.darkMode);
            })
            .enabled(ent1.enabled)
            .onClick((): void => {
              if (ent1.enabled && ent1.itemId !== 0) {
                ctx.onSelect(ent1.itemId);
              }
            })
          }
        }, (ent1: MenuEntryModel): string => ent1.key)
      }
    }, (sec1: MenuSectionModel): string => sec1.key)
  }
  .width(ctx.root.widthVp)
  .radius(8)
  .padding(4)
  .menuItemGroupDivider({
    strokeWidth: LengthMetrics.px(1),
    color: ctx.darkMode ? '#24FFFFFF' : '#1A000000',
    startMargin: LengthMetrics.vp(8),
    endMargin: LengthMetrics.vp(8),
    mode: DividerMode.EMBEDDED_IN_MENU
  })
  .direction(ctx.direction)
}

@Builder
function buildMessageBoxDialog(ctx: MessageBoxContext): void {
  Column() {
    if (ctx.title.length > 0) {
      Text(ctx.title)
        .fontSize(18)
        .fontWeight(FontWeight.Bold)
        .fontColor(ctx.darkMode ? '#FFFFFF' : '#182431')
        .width('100%')
        .margin({ bottom: 8 })
    }
    if (ctx.message.length > 0) {
      Text(ctx.message)
        .fontSize(15)
        .fontWeight(ctx.title.length > 0 ? FontWeight.Medium : FontWeight.Bold)
        .fontColor(ctx.darkMode ? '#F1F3F5' : '#182431')
        .width('100%')
        .margin({ bottom: ctx.detail.length > 0 ? 8 : 16 })
    }
    if (ctx.detail.length > 0) {
      Text(ctx.detail)
        .fontSize(13)
        .fontColor(ctx.darkMode ? '#A6ADB4' : '#666E77')
        .width('100%')
        .margin({ bottom: 16 })
    }
    if (ctx.checkboxLabel.length > 0) {
      Row() {
        Checkbox({ name: 'msgbox_chk' })
          .select(ctx.checkboxChecked)
          .onChange((val: boolean): void => {
            ctx.onToggleCheckbox(val);
          })
          .margin({ right: 8 })
        Text(ctx.checkboxLabel)
          .fontSize(14)
          .fontColor(ctx.darkMode ? '#E5E7EB' : '#182431')
          .layoutWeight(1)
      }
      .width('100%')
      .alignItems(VerticalAlign.Center)
      .margin({ bottom: 18 })
    }
    Flex({ wrap: FlexWrap.Wrap, justifyContent: FlexAlign.End }) {
      ForEach(ctx.buttons, (btn: MessageBoxButtonModel) => {
        Button(btn.label)
          .type(ButtonType.Capsule)
          .fontSize(14)
          .height(36)
          .padding({ left: 18, right: 18 })
          .margin({ left: 8, top: 4 })
          .backgroundColor(
            btn.isDestructive
              ? '#E84026'
              : (btn.isDefault ? '#0A59F7' : (ctx.darkMode ? '#2E3238' : '#E6E9ED'))
          )
          .fontColor(
            (btn.isDestructive || btn.isDefault)
              ? '#FFFFFF'
              : (ctx.darkMode ? '#F1F3F5' : '#182431')
          )
          .defaultFocus(btn.isDefault)
          .onClick((): void => {
            ctx.onChoose(btn.index);
          })
      }, (btn: MessageBoxButtonModel): string => btn.key)
    }
    .width('100%')
  }
  .width(400)
  .padding(24)
  .borderRadius(20)
  .backgroundColor(ctx.darkMode ? '#1F2227' : '#FFFFFF')
}

@Entry
@Component
struct Index {
  private content: NodeContent = new NodeContent();
  private winId: number = 1;
  private isWebWindow: boolean = false;
  private webIncognito: boolean = false;
  private bgHex: string = '#FFF4F4F6';
  private bgARGB: number = 0xFFF4F4F6;
  private webCtrl: webview.WebviewController = new webview.WebviewController();
  private webAttached: boolean = false;
  private hasRealNavigation: boolean = false;
  private queuedWebCmds: QueuedWebCmd[] = [];
  private activeMenuNode: ComponentContent<PopupMenuContext> | undefined = undefined;
  private activeMenuReqId: number = 0;
  private activeMsgNode: ComponentContent<MessageBoxContext> | undefined = undefined;
  private activeMsgReqId: number = 0;
  private activeMsgCancelId: number = 0;
  private activeMsgChecked: boolean = false;

  aboutToAppear(): void {
    const uiCtx: UIContext = this.getUIContext();
    const sharedStorage: LocalStorage | undefined = uiCtx.getSharedLocalStorage();
    this.winId = sharedStorage?.get<number>('winId') ?? 1;
    const arkWinId: number = sharedStorage?.get<number>('arkWindowId') ?? hostBridge.getArkWindowId(this.winId);
    const darkMode: boolean = AppStorage.get<boolean>('darkMode') ?? false;

    this.isWebWindow = sharedStorage?.get<boolean>('isWebWindow') ?? false;
    this.webIncognito = sharedStorage?.get<boolean>('webIncognito') ?? false;
    const defaultHex: string = darkMode ? '#FF1C1C1F' : (this.isWebWindow ? '#FFFFFFFF' : '#FFF4F4F6');
    const defaultARGB: number = darkMode ? 0xFF1C1C1F : (this.isWebWindow ? 0xFFFFFFFF : 0xFFF4F4F6);
    this.bgHex = sharedStorage?.get<string>('bgHex') ?? defaultHex;
    this.bgARGB = sharedStorage?.get<number>('bgARGB') ?? defaultARGB;

    hostBridge.registerPopupMenuOpener(
      this.winId,
      (reqId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void => {
        this.openNativePopupMenu(reqId, x, y, rtl, items);
      }
    );
    hostBridge.registerMessageBoxOpener(
      this.winId,
      (box: HostMessageBox): void => {
        this.openNativeMessageBox(box);
      }
    );

    if (this.isWebWindow) {
      hostBridge.registerWebRunner(
        this.winId,
        (op: number, arg1: string, arg2: string): void => {
          this.runWebCommand(op, arg1, arg2);
        }
      );
      return;
    }

    let densityPixels: number = 1.5;
    let refreshRate: number = 120;
    try {
      const d: display.Display = display.getDefaultDisplaySync();
      if (d.densityPixels > 0) {
        densityPixels = d.densityPixels;
      }
      if (d.refreshRate > 0) {
        refreshRate = d.refreshRate;
      }
    } catch (_) {}

    host.bindWindow(this.winId, uiCtx, this.content, arkWinId, densityPixels, refreshRate, darkMode, this.bgARGB);
  }

  aboutToDisappear(): void {
    this.dismissActiveMenu(0);
    this.dismissActiveMessageBox();
    hostBridge.unregisterPopupMenuOpener(this.winId);
    hostBridge.unregisterMessageBoxOpener(this.winId);
    if (this.isWebWindow) {
      hostBridge.unregisterWebRunner(this.winId);
      this.webAttached = false;
      this.queuedWebCmds = [];
      return;
    }
    host.unbindWindow(this.winId);
  }

  private safeCanBack(): boolean {
    if (!this.webAttached) {
      return false;
    }
    try {
      return this.webCtrl.accessBackward();
    } catch (_) {
      return false;
    }
  }

  private safeCanForward(): boolean {
    if (!this.webAttached) {
      return false;
    }
    try {
      return this.webCtrl.accessForward();
    } catch (_) {
      return false;
    }
  }

  private onWebAttached(): void {
    this.webAttached = true;
    const ua: string = hostBridge.getWebUserAgent(this.winId);
    if (ua.length > 0) {
      try {
        this.webCtrl.setCustomUserAgent(ua);
      } catch (_) {}
    }
    const pendingHtml: [string, string] = hostBridge.consumePendingWebHtml(this.winId);
    const initialUrl: string = hostBridge.getWebUrl(this.winId);
    if (pendingHtml[0].length > 0) {
      this.hasRealNavigation = true;
      const base: string | undefined = pendingHtml[1].length > 0 ? pendingHtml[1] : undefined;
      try {
        this.webCtrl.loadData(pendingHtml[0], 'text/html', 'UTF-8', base, base);
      } catch (_) {}
    } else if (initialUrl.length > 0) {
      this.hasRealNavigation = true;
      try {
        this.webCtrl.loadUrl(initialUrl);
      } catch (_) {}
    }

    const pending: QueuedWebCmd[] = this.queuedWebCmds.slice();
    this.queuedWebCmds = [];
    for (let i: number = 0; i < pending.length; i++) {
      this.executeAttachedWebCmd(pending[i].op, pending[i].arg1, pending[i].arg2);
    }
  }

  private runWebCommand(op: number, arg1: string, arg2: string): void {
    if (!this.webAttached) {
      if (op === WEB_OP_LOAD_URL || op === WEB_OP_SET_UA || op === WEB_OP_LOAD_HTML) {
        return;
      }
      const cmd: QueuedWebCmd = new QueuedWebCmd();
      cmd.op = op;
      cmd.arg1 = arg1;
      cmd.arg2 = arg2;
      this.queuedWebCmds.push(cmd);
      return;
    }
    this.executeAttachedWebCmd(op, arg1, arg2);
  }

  private executeAttachedWebCmd(op: number, arg1: string, arg2: string): void {
    try {
      switch (op) {
        case WEB_OP_LOAD_URL:
          if (arg1.length > 0) {
            this.hasRealNavigation = true;
            this.webCtrl.loadUrl(arg1);
          }
          break;
        case WEB_OP_LOAD_HTML: {
          this.hasRealNavigation = true;
          const base: string | undefined = arg2.length > 0 ? arg2 : undefined;
          this.webCtrl.loadData(arg1, 'text/html', 'UTF-8', base, base);
          break;
        }
        case WEB_OP_RELOAD:
          this.webCtrl.refresh();
          break;
        case WEB_OP_STOP:
          this.webCtrl.stop();
          break;
        case WEB_OP_BACK:
          if (this.webCtrl.accessBackward()) {
            this.webCtrl.backward();
          }
          break;
        case WEB_OP_FORWARD:
          if (this.webCtrl.accessForward()) {
            this.webCtrl.forward();
          }
          break;
        case WEB_OP_EVAL:
          if (arg1.length > 0) {
            this.webCtrl.runJavaScript(arg1).catch((): void => {});
          }
          break;
        case WEB_OP_SET_UA:
          this.webCtrl.setCustomUserAgent(arg1);
          break;
        case WEB_OP_CLEAR_CACHE:
          this.webCtrl.removeCache(true);
          break;
        default:
          break;
      }
    } catch (_) {}
  }

  private dismissActiveMenu(chosenId: number): void {
    const reqId: number = this.activeMenuReqId;
    const node: ComponentContent<PopupMenuContext> | undefined = this.activeMenuNode;
    this.activeMenuReqId = 0;
    this.activeMenuNode = undefined;
    if (reqId !== 0) {
      host.resolvePopupMenu(reqId, chosenId);
    }
    if (node !== undefined) {
      this.getUIContext().getPromptAction().closeMenu(node).then((): void => {
        node.dispose();
      }).catch((): void => {
        node.dispose();
      });
    }
  }

  private dismissActiveMessageBox(): void {
    const reqId: number = this.activeMsgReqId;
    const node: ComponentContent<MessageBoxContext> | undefined = this.activeMsgNode;
    const cancelId: number = this.activeMsgCancelId;
    const checked: boolean = this.activeMsgChecked;
    this.activeMsgReqId = 0;
    this.activeMsgNode = undefined;
    if (reqId !== 0) {
      host.resolveDialog(reqId, cancelId, checked, '', []);
    }
    if (node !== undefined) {
      this.getUIContext().getPromptAction().closeCustomDialog(node).then((): void => {
        node.dispose();
      }).catch((): void => {
        node.dispose();
      });
    }
  }

  private openNativeMessageBox(box: HostMessageBox): void {
    this.dismissActiveMessageBox();
    const uiCtx: UIContext = this.getUIContext();
    let settled: boolean = false;
    let currentChecked: boolean = box.checkboxChecked;

    const finish = (response: number, closeExplicitly: boolean): void => {
      if (settled) {
        return;
      }
      settled = true;
      if (this.activeMsgReqId === box.reqId) {
        this.activeMsgReqId = 0;
        const curNode: ComponentContent<MessageBoxContext> | undefined = this.activeMsgNode;
        this.activeMsgNode = undefined;
        if (curNode !== undefined) {
          if (closeExplicitly) {
            uiCtx.getPromptAction().closeCustomDialog(curNode).then((): void => {
              curNode.dispose();
            }).catch((): void => {
              curNode.dispose();
            });
          } else {
            curNode.dispose();
          }
        }
      }
      host.resolveDialog(box.reqId, response, currentChecked, '', []);
    };

    const ctx: MessageBoxContext = new MessageBoxContext();
    ctx.title = box.title;
    ctx.message = box.message;
    ctx.detail = box.detail;
    ctx.checkboxLabel = box.checkboxLabel;
    ctx.checkboxChecked = box.checkboxChecked;
    ctx.darkMode = AppStorage.get<boolean>('darkMode') ?? false;
    for (let i: number = 0; i < box.buttons.length; i++) {
      const bm: MessageBoxButtonModel = new MessageBoxButtonModel();
      bm.key = 'mb_btn_' + box.reqId.toString() + '_' + i.toString();
      bm.index = i;
      bm.label = box.buttons[i];
      bm.isDefault = i === box.defaultId;
      bm.isDestructive = box.destructiveIds.indexOf(i) >= 0;
      ctx.buttons.push(bm);
    }
    ctx.onToggleCheckbox = (checked: boolean): void => {
      currentChecked = checked;
      this.activeMsgChecked = checked;
    };
    ctx.onChoose = (idx: number): void => {
      finish(idx, true);
    };

    const contentNode: ComponentContent<MessageBoxContext> = new ComponentContent<MessageBoxContext>(
      uiCtx,
      wrapBuilder<[MessageBoxContext]>(buildMessageBoxDialog),
      ctx
    );
    this.activeMsgReqId = box.reqId;
    this.activeMsgCancelId = box.cancelId;
    this.activeMsgChecked = box.checkboxChecked;
    this.activeMsgNode = contentNode;

    uiCtx.getPromptAction().openCustomDialog(contentNode, {
      alignment: DialogAlignment.Center,
      isModal: true,
      autoCancel: false,
      onWillDismiss: (dismissAction: DismissDialogAction): void => {
        dismissAction.dismiss();
        finish(box.cancelId, false);
      }
    }).catch((): void => {
      finish(box.cancelId, false);
    });
  }

  private openNativePopupMenu(reqId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void {
    this.dismissActiveMenu(0);
    const uiCtx: UIContext = this.getUIContext();
    const rootLevel: MenuLevelModel = buildMenuLevel(uiCtx, items, 'req_' + reqId.toString());
    if (rootLevel.sections.length === 0) {
      host.resolvePopupMenu(reqId, 0);
      return;
    }

    let settled: boolean = false;
    const finish = (chosenId: number, closeExplicitly: boolean): void => {
      if (settled) {
        return;
      }
      settled = true;
      if (this.activeMenuReqId === reqId) {
        this.activeMenuReqId = 0;
        const curNode: ComponentContent<PopupMenuContext> | undefined = this.activeMenuNode;
        this.activeMenuNode = undefined;
        if (curNode !== undefined) {
          if (closeExplicitly) {
            uiCtx.getPromptAction().closeMenu(curNode).then((): void => {
              curNode.dispose();
            }).catch((): void => {
              curNode.dispose();
            });
          } else {
            curNode.dispose();
          }
        }
      }
      host.resolvePopupMenu(reqId, chosenId);
    };

    const ctx: PopupMenuContext = new PopupMenuContext();
    ctx.root = rootLevel;
    ctx.darkMode = AppStorage.get<boolean>('darkMode') ?? false;
    ctx.direction = rtl ? Direction.Rtl : Direction.Ltr;
    ctx.onSelect = (chosenId: number): void => {
      finish(chosenId, true);
    };

    const contentNode: ComponentContent<PopupMenuContext> = new ComponentContent<PopupMenuContext>(
      uiCtx,
      wrapBuilder<[PopupMenuContext]>(buildPopupMenuRoot),
      ctx
    );
    this.activeMenuReqId = reqId;
    this.activeMenuNode = contentNode;

    uiCtx.getPromptAction().openMenu(
      contentNode,
      { id: this.rootStackId() },
      {
        anchorPosition: { x: Math.max(0, x), y: Math.max(0, y) },
        enableArrow: false,
        showInSubWindow: this.winId === 1,
        onDisappear: (): void => {
          finish(0, false);
        }
      }
    ).catch((): void => {
      finish(0, false);
    });
  }

  private rootStackId(): string {
    return 'mygo_root_' + this.winId.toString();
  }

  build() {
    Stack({ alignContent: Alignment.TopStart }) {
      if (this.isWebWindow) {
        Web({
          src: '',
          controller: this.webCtrl,
          incognitoMode: this.webIncognito
        })
          .width('100%')
          .height('100%')
          .backgroundColor(this.bgHex)
          .javaScriptAccess(true)
          .domStorageAccess(true)
          .databaseAccess(true)
          .imageAccess(true)
          .onlineImageAccess(true)
          .mixedMode(MixedMode.All)
          .darkMode(WebDarkMode.Auto)
          .zoomAccess(true)
          .onControllerAttached((): void => {
            this.onWebAttached();
          })
          .onPageBegin((ev: OnPageBeginEvent): void => {
            if (ev.url === 'about:blank' && !this.hasRealNavigation) {
              return;
            }
            host.notifyWebEvent(
              this.winId,
              WEB_EV_PAGE_BEGIN,
              this.safeCanBack(),
              this.safeCanForward(),
              0,
              ev.url ?? '',
              ''
            );
          })
          .onPageEnd((ev: OnPageEndEvent): void => {
            if (ev.url === 'about:blank' && !this.hasRealNavigation) {
              return;
            }
            host.notifyWebEvent(
              this.winId,
              WEB_EV_PAGE_END,
              this.safeCanBack(),
              this.safeCanForward(),
              0,
              ev.url ?? '',
              ''
            );
          })
          .onTitleReceive((ev: OnTitleReceiveEvent): void => {
            const t: string = (ev.title ?? '').trim();
            if (t.length === 0 || t === 'about:blank') {
              return;
            }
            host.notifyWebEvent(
              this.winId,
              WEB_EV_TITLE,
              this.safeCanBack(),
              this.safeCanForward(),
              0,
              t,
              ''
            );
          })
          .onErrorReceive((ev: OnErrorReceiveEvent): void => {
            let mainFrame: boolean = false;
            let reqUrl: string = '';
            let code: number = -1;
            let info: string = '';
            try {
              mainFrame = ev.request.isMainFrame();
              reqUrl = ev.request.getRequestUrl();
            } catch (_) {}
            if (!mainFrame) {
              return;
            }
            if (reqUrl === 'about:blank' && !this.hasRealNavigation) {
              return;
            }
            try {
              code = ev.error.getErrorCode();
              info = ev.error.getErrorInfo();
            } catch (_) {}
            host.notifyWebEvent(
              this.winId,
              WEB_EV_ERROR,
              this.safeCanBack(),
              this.safeCanForward(),
              code,
              reqUrl,
              info
            );
          })
      } else {
        ContentSlot(this.content)
      }
    }
    .id(this.rootStackId())
    .width('100%')
    .height('100%')
    .backgroundColor(this.bgHex)
  }
}
