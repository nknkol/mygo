import host, { HostMenuItem, HostMessageBox } from 'libmygo_host.so';
import { ComponentContent, NodeContent, UIContext, display } from '@kit.ArkUI';
import { hostBridge } from '../bridge/HostBridge';

const MENU_KIND_SEPARATOR: number = 3;
const MENU_KIND_SUBMENU: number = 4;

class MenuEntryModel {
  key: string = '';
  itemId: number = 0;
  label: string = '';
  labelInfo: string | undefined = undefined;
  enabled: boolean = true;
  checked: boolean = false;
  hasSubmenu: boolean = false;
  subSections: MenuSectionModel[] = [];
}

class MenuSectionModel {
  key: string = '';
  entries: MenuEntryModel[] = [];
}

class PopupMenuContext {
  sections: MenuSectionModel[] = [];
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

function buildMenuSections(items: HostMenuItem[], prefix: string): MenuSectionModel[] {
  const sections: MenuSectionModel[] = [];
  let current: MenuEntryModel[] = [];
  let secIdx: number = 0;
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
    if (raw.accel.length > 0) {
      ent.labelInfo = raw.accel;
    }
    ent.enabled = raw.enabled;
    ent.checked = raw.checked;
    if (raw.kind === MENU_KIND_SUBMENU && raw.items.length > 0) {
      ent.subSections = buildMenuSections(raw.items, ent.key);
      ent.hasSubmenu = ent.subSections.length > 0;
    }
    current.push(ent);
  }
  if (current.length > 0) {
    const lastSec: MenuSectionModel = new MenuSectionModel();
    lastSec.key = prefix + '_sec_' + secIdx.toString();
    lastSec.entries = current;
    sections.push(lastSec);
  }
  return sections;
}

@Builder
function buildLevel3SubMenu(sections: MenuSectionModel[], ctx: PopupMenuContext): void {
  Menu() {
    ForEach(sections, (sec3: MenuSectionModel) => {
      MenuItemGroup() {
        ForEach(sec3.entries, (ent3: MenuEntryModel) => {
          MenuItem({
            content: ent3.label,
            labelInfo: ent3.labelInfo
          })
          .enabled(ent3.enabled)
          .selected(ent3.checked)
          .selectIcon(ent3.checked)
          .onClick((): void => {
            if (ent3.enabled && ent3.itemId !== 0) {
              ctx.onSelect(ent3.itemId);
            }
          })
        }, (ent3: MenuEntryModel): string => ent3.key)
      }
    }, (sec3: MenuSectionModel): string => sec3.key)
  }
  .direction(ctx.direction)
}

@Builder
function buildLevel2SubMenu(sections: MenuSectionModel[], ctx: PopupMenuContext): void {
  Menu() {
    ForEach(sections, (sec2: MenuSectionModel) => {
      MenuItemGroup() {
        ForEach(sec2.entries, (ent2: MenuEntryModel) => {
          if (ent2.hasSubmenu) {
            MenuItem({
              content: ent2.label,
              labelInfo: ent2.labelInfo,
              builder: (): void => {
                buildLevel3SubMenu(ent2.subSections, ctx);
              }
            })
            .enabled(ent2.enabled)
          } else {
            MenuItem({
              content: ent2.label,
              labelInfo: ent2.labelInfo
            })
            .enabled(ent2.enabled)
            .selected(ent2.checked)
            .selectIcon(ent2.checked)
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
  .direction(ctx.direction)
}

@Builder
function buildPopupMenuRoot(ctx: PopupMenuContext): void {
  Menu() {
    ForEach(ctx.sections, (sec1: MenuSectionModel) => {
      MenuItemGroup() {
        ForEach(sec1.entries, (ent1: MenuEntryModel) => {
          if (ent1.hasSubmenu) {
            MenuItem({
              content: ent1.label,
              labelInfo: ent1.labelInfo,
              builder: (): void => {
                buildLevel2SubMenu(ent1.subSections, ctx);
              }
            })
            .enabled(ent1.enabled)
          } else {
            MenuItem({
              content: ent1.label,
              labelInfo: ent1.labelInfo
            })
            .enabled(ent1.enabled)
            .selected(ent1.checked)
            .selectIcon(ent1.checked)
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
    const darkMode: boolean = AppStorage.get<boolean>('darkMode') ?? false;

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
    host.bindWindow(this.winId, uiCtx, this.content, arkWinId, densityPixels, refreshRate, darkMode);
  }

  aboutToDisappear(): void {
    this.dismissActiveMenu(0);
    this.dismissActiveMessageBox();
    hostBridge.unregisterPopupMenuOpener(this.winId);
    hostBridge.unregisterMessageBoxOpener(this.winId);
    host.unbindWindow(this.winId);
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
    const sections: MenuSectionModel[] = buildMenuSections(items, 'req_' + reqId.toString());
    if (sections.length === 0) {
      host.resolvePopupMenu(reqId, 0);
      return;
    }

    const uiCtx: UIContext = this.getUIContext();
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
    ctx.sections = sections;
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
      ContentSlot(this.content)
    }
    .id(this.rootStackId())
    .width('100%')
    .height('100%')
    .backgroundColor('#000000')
  }
}
