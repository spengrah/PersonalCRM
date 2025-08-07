export default function Home() {
  return (
    <div className="min-h-screen bg-gray-50 dark:bg-gray-900">
      <div className="container mx-auto px-4 py-8">
        <header className="text-center mb-8">
          <h1 className="text-4xl font-bold text-gray-900 dark:text-white mb-2">
            Personal CRM
          </h1>
          <p className="text-gray-600 dark:text-gray-300">
            Your personal relationship management system
          </p>
        </header>
        
        <main className="max-w-4xl mx-auto">
          <div className="bg-white dark:bg-gray-800 rounded-lg shadow-md p-6">
            <h2 className="text-2xl font-semibold mb-4 text-gray-800 dark:text-gray-200">
              Getting Started
            </h2>
            <div className="grid md:grid-cols-2 gap-6">
              <div className="space-y-4">
                <div className="p-4 bg-blue-50 dark:bg-blue-900/20 rounded-lg">
                  <h3 className="font-semibold text-blue-900 dark:text-blue-100">
                    Contacts
                  </h3>
                  <p className="text-blue-700 dark:text-blue-300 text-sm">
                    Manage your personal and professional contacts
                  </p>
                </div>
                <div className="p-4 bg-green-50 dark:bg-green-900/20 rounded-lg">
                  <h3 className="font-semibold text-green-900 dark:text-green-100">
                    Reminders
                  </h3>
                  <p className="text-green-700 dark:text-green-300 text-sm">
                    Stay connected with automated follow-up reminders
                  </p>
                </div>
              </div>
              <div className="space-y-4">
                <div className="p-4 bg-purple-50 dark:bg-purple-900/20 rounded-lg">
                  <h3 className="font-semibold text-purple-900 dark:text-purple-100">
                    Notes & Interactions
                  </h3>
                  <p className="text-purple-700 dark:text-purple-300 text-sm">
                    Keep track of conversations and interactions
                  </p>
                </div>
                <div className="p-4 bg-orange-50 dark:bg-orange-900/20 rounded-lg">
                  <h3 className="font-semibold text-orange-900 dark:text-orange-100">
                    AI Assistant
                  </h3>
                  <p className="text-orange-700 dark:text-orange-300 text-sm">
                    Get insights and suggestions from your data
                  </p>
                </div>
              </div>
            </div>
          </div>
        </main>
      </div>
    </div>
  );
}