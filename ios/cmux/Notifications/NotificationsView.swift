import SwiftUI

struct NotificationsView: View {
    @EnvironmentObject var appState: AppState

    var body: some View {
        NavigationStack {
            Group {
                if appState.notifications.isEmpty {
                    ContentUnavailableView(
                        "No notifications",
                        systemImage: "bell.slash",
                        description: Text("New notifications will appear here.")
                    )
                } else {
                    List(appState.notifications) { notification in
                        NotificationRow(notification: notification)
                    }
                    .listStyle(.plain)
                }
            }
            .navigationTitle("Notifications")
            .toolbar {
                if !appState.notifications.isEmpty {
                    ToolbarItem(placement: .topBarTrailing) {
                        Button("Clear all") { appState.clearNotifications() }
                            .foregroundStyle(.red)
                    }
                }
            }
        }
    }
}
